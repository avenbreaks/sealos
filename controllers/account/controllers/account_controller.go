/*
Copyright 2022 labring.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/rest"

	"k8s.io/client-go/kubernetes"

	"github.com/go-logr/logr"
	gonanoid "github.com/matoous/go-nanoid/v2"

	accountv1 "github.com/labring/sealos/controllers/account/api/v1"
	"github.com/labring/sealos/controllers/pkg/crypto"
	"github.com/labring/sealos/controllers/pkg/database"
	"github.com/labring/sealos/controllers/pkg/pay"
	"github.com/labring/sealos/controllers/pkg/resources"
	pkgtypes "github.com/labring/sealos/controllers/pkg/types"
	"github.com/labring/sealos/controllers/pkg/utils/env"
	"github.com/labring/sealos/controllers/pkg/utils/retry"
	userv1 "github.com/labring/sealos/controllers/user/api/v1"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	cretry "k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	ACCOUNTNAMESPACEENV          = "ACCOUNT_NAMESPACE"
	DEFAULTACCOUNTNAMESPACE      = "sealos-system"
	AccountAnnotationNewAccount  = "account.sealos.io/new-account"
	AccountAnnotationIgnoreQuota = "account.sealos.io/ignore-quota"
	NEWACCOUNTAMOUNTENV          = "NEW_ACCOUNT_AMOUNT"
	RECHARGEGIFT                 = "recharge-gift"
	SEALOS                       = "sealos"
	DefaultInitialBalance        = 5_000_000
)

// AccountReconciler reconciles an Account object
type AccountReconciler struct {
	client.Client
	Scheme                 *runtime.Scheme
	Logger                 logr.Logger
	AccountSystemNamespace string
	DBClient               database.Account
	MongoDBURI             string
	Activities             pkgtypes.Activities
	RechargeStep           []int64
	RechargeRatio          []float64
}

//+kubebuilder:rbac:groups=account.sealos.io,resources=accounts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=account.sealos.io,resources=accounts/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=account.sealos.io,resources=accounts/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=resourcequotas,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=limitranges,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=user.sealos.io,resources=users,verbs=get;list;watch
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

func (r *AccountReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	//It should not stop the normal process for the failure to delete the payment
	// delete payments that exist for more than 5 minutes
	if err := r.DeletePayment(ctx); err != nil {
		r.Logger.Error(err, "delete payment failed")
	}
	user := &userv1.User{}
	owner := ""
	if err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.Name}, user); err == nil {
		if owner = user.Annotations[userv1.UserAnnotationOwnerKey]; owner == "" {
			return ctrl.Result{}, fmt.Errorf("user owner is empty")
		}
		// This is only used to monitor and initialize user resource creation data,
		// determine the resource quota created by the owner user and the resource quota initialized by the account user,
		// and only the resource quota created by the team user
		_, err = r.syncAccount(ctx, owner, r.AccountSystemNamespace, "ns-"+user.Name)
		return ctrl.Result{}, err
	} else if client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}

	payment := &accountv1.Payment{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.Name}, payment); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if payment.Spec.UserID == "" || payment.Spec.Amount == 0 {
		return ctrl.Result{}, fmt.Errorf("payment is invalid: %v", payment)
	}
	if payment.Status.TradeNO == "" {
		return ctrl.Result{Requeue: true, RequeueAfter: time.Millisecond * 300}, nil
	}
	if payment.Status.Status == pay.PaymentSuccess {
		return ctrl.Result{}, nil
	}

	account, err := r.syncAccount(ctx, getUsername(payment.Spec.UserID), r.AccountSystemNamespace, payment.Namespace)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get account failed: %v", err)
	}

	// get payment handler
	payHandler, err := pay.NewPayHandler(payment.Spec.PaymentMethod)
	if err != nil {
		r.Logger.Error(err, "get payment handler failed")
		return ctrl.Result{}, err
	}
	// get payment details(status, amount)
	// TODO The GetPaymentDetails may cause issues when using Stripe
	status, orderAmount, err := payHandler.GetPaymentDetails(payment.Status.TradeNO)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("query order failed: %v", err)
	}
	r.Logger.V(1).Info("query order details", "orderStatus", status, "orderAmount", orderAmount)
	switch status {
	case pay.PaymentSuccess:
		now := time.Now().UTC()
		//1¥ = 100WechatPayAmount; 1 WechatPayAmount = 10000 SealosAmount
		payAmount := orderAmount * 10000
		updateAnno, gift, err := r.getAmountWithRates(payAmount, account)
		if err != nil {
			r.Logger.Error(err, "get gift error")
		}
		err = crypto.RechargeBalance(account.Status.EncryptBalance, gift)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("recharge encrypt balance failed: %v", err)
		}
		if err := SyncAccountStatus(ctx, r.Client, account); err != nil {
			return ctrl.Result{}, fmt.Errorf("update account status failed: %v", err)
		}
		payment.Status.Status = pay.PaymentSuccess
		if err := r.Status().Update(ctx, payment); err != nil {
			return ctrl.Result{}, fmt.Errorf("update payment failed: %v", err)
		}
		if len(updateAnno) > 0 {
			account.Annotations = updateAnno
			if err := r.Update(ctx, account); err != nil {
				return ctrl.Result{}, fmt.Errorf("update account failed: %v", err)
			}
		}

		id, err := gonanoid.New(12)
		if err != nil {
			r.Logger.Error(err, "create id failed", "id", id, "payment", payment)
			return ctrl.Result{}, nil
		}
		err = r.DBClient.SaveBillings(&resources.Billing{
			Time:      now,
			OrderID:   id,
			Amount:    gift,
			Namespace: payment.Namespace,
			Owner:     getUsername(payment.Spec.UserID),
			Type:      accountv1.Recharge,
			Payment: &resources.Payment{
				Method:  payment.Spec.PaymentMethod,
				TradeNO: payment.Status.TradeNO,
				CodeURL: payment.Status.CodeURL,
				UserID:  payment.Spec.UserID,
				Amount:  payAmount,
			},
		})
		if err != nil {
			r.Logger.Error(err, "save billings failed", "id", id, "payment", payment)
			return ctrl.Result{}, nil
		}
	case pay.PaymentProcessing, pay.PaymentNotPaid:
		return ctrl.Result{Requeue: true, RequeueAfter: time.Second}, nil
	case pay.PaymentFailed, pay.PaymentExpired:
		if err := r.Delete(ctx, payment); err != nil {
			return ctrl.Result{}, fmt.Errorf("delete payment failed: %v", err)
		}
		return ctrl.Result{}, nil
	default:
		return ctrl.Result{}, fmt.Errorf("unknown status: %v", err)
	}

	return ctrl.Result{}, nil
}

func (r *AccountReconciler) syncAccount(ctx context.Context, owner, accountNamespace string, userNamespace string) (*accountv1.Account, error) {
	if err := r.syncResourceQuotaAndLimitRange(ctx, userNamespace); err != nil {
		r.Logger.Error(err, "sync resource resourceQuota and limitRange failed")
	}
	if err := r.adaptEphemeralStorageLimitRange(ctx, userNamespace); err != nil {
		r.Logger.Error(err, "adapt ephemeral storage limitRange failed")
	}
	account := accountv1.Account{
		ObjectMeta: metav1.ObjectMeta{
			Name:      owner,
			Namespace: accountNamespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, &account, func() error {
		if account.Annotations == nil {
			account.Annotations = make(map[string]string)
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("failed to create account %v, err: %v", account, err)
	}
	// If the user is not the owner, the user represents the team and does not perform subsequent account initialization operations
	if owner != getUsername(userNamespace) {
		return &account, nil
	}
	// add role get account permission
	if err := r.syncRoleAndRoleBinding(ctx, owner, userNamespace); err != nil {
		return nil, fmt.Errorf("sync role and rolebinding failed: %v", err)
	}
	err := initBalance(&account)
	if err != nil {
		return nil, fmt.Errorf("sync init balance failed: %v", err)
	}
	// add account balance when account is new user
	stringAmount := os.Getenv(NEWACCOUNTAMOUNTENV)
	if stringAmount == "" {
		r.Logger.V(1).Info("NEWACCOUNTAMOUNTENV is empty", "account", account)
		return &account, nil
	}

	if account.Annotations[AccountAnnotationNewAccount] == "false" {
		//r.Logger.V(1).Info("account is not a new user ", "account", account)
		return &account, nil
	}

	amount, err := crypto.DecryptInt64(stringAmount)
	if err != nil {
		r.Logger.Error(err, "decrypt amount failed", "amount", stringAmount)
		amount = DefaultInitialBalance
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, &account, func() error {
		if account.Annotations == nil {
			account.Annotations = make(map[string]string)
		}
		account.Annotations[AccountAnnotationNewAccount] = "false"
		return nil
	}); err != nil {
		return nil, err
	}
	err = initBalance(&account)
	if err != nil {
		return nil, fmt.Errorf("sync init balance failed: %v", err)
	}
	err = crypto.RechargeBalance(account.Status.EncryptBalance, amount)
	if err != nil {
		return nil, fmt.Errorf("recharge balance failed: %v", err)
	}
	if err := SyncAccountStatus(ctx, r.Client, &account); err != nil {
		return nil, fmt.Errorf("update account failed: %v", err)
	}
	r.Logger.Info("account created,will charge new account some money", "account", account, "stringAmount", stringAmount)

	return &account, nil
}

func (r *AccountReconciler) syncResourceQuotaAndLimitRange(ctx context.Context, nsName string) error {
	objs := []client.Object{client.Object(resources.GetDefaultLimitRange(nsName, nsName)), client.Object(resources.GetDefaultResourceQuota(nsName, ResourceQuotaPrefix+nsName))}
	for i := range objs {
		err := retry.Retry(10, 1*time.Second, func() error {
			_, err := controllerutil.CreateOrUpdate(ctx, r.Client, objs[i], func() error {
				return nil
			})
			return err
		})
		if err != nil {
			return fmt.Errorf("sync resource %T failed: %v", objs[i], err)
		}
	}
	return nil
}

func (r *AccountReconciler) adaptEphemeralStorageLimitRange(ctx context.Context, nsName string) error {
	limit := resources.GetDefaultLimitRange(nsName, nsName)
	return retry.Retry(10, 1*time.Second, func() error {
		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, limit, func() error {
			if len(limit.Spec.Limits) == 0 {
				limit = resources.GetDefaultLimitRange(nsName, nsName)
			}
			limit.Spec.Limits[0].DefaultRequest[corev1.ResourceEphemeralStorage] = resources.LimitRangeDefault[corev1.ResourceEphemeralStorage]
			limit.Spec.Limits[0].Default[corev1.ResourceEphemeralStorage] = resources.LimitRangeDefault[corev1.ResourceEphemeralStorage]
			//if _, ok := limit.Spec.Limits[0].Default[corev1.ResourceEphemeralStorage]; !ok {
			//}
			return nil
		})
		return err
	})
}

func (r *AccountReconciler) syncRoleAndRoleBinding(ctx context.Context, name, namespace string) error {
	role := rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "userAccountRole-" + name,
			Namespace: r.AccountSystemNamespace,
		},
	}
	err := cretry.RetryOnConflict(cretry.DefaultRetry, func() error {
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, &role, func() error {
			role.Rules = []rbacv1.PolicyRule{
				{
					APIGroups:     []string{"account.sealos.io"},
					Resources:     []string{"accounts"},
					Verbs:         []string{"get", "watch", "list"},
					ResourceNames: []string{name},
				},
			}
			return nil
		}); err != nil {
			return fmt.Errorf("create role failed: %v,username: %v,namespace: %v", err, name, namespace)
		}
		return nil
	})
	if err != nil {
		return err
	}
	roleBinding := rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "userAccountRoleBinding-" + name,
			Namespace: r.AccountSystemNamespace,
		},
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, &roleBinding, func() error {
		roleBinding.RoleRef = rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     role.Name,
		}
		roleBinding.Subjects = []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      name,
				Namespace: namespace,
			},
		}

		return nil
	}); err != nil {
		return fmt.Errorf("create roleBinding failed: %v,rolename: %v,username: %v,ns: %v", err, role.Name, name, namespace)
	}
	return nil
}

// DeletePayment delete payments that exist for more than 5 minutes
func (r *AccountReconciler) DeletePayment(ctx context.Context) error {
	payments := &accountv1.PaymentList{}
	err := r.List(ctx, payments)
	if err != nil {
		return err
	}
	for _, payment := range payments.Items {
		//get payment handler
		payHandler, err := pay.NewPayHandler(payment.Spec.PaymentMethod)
		if err != nil {
			r.Logger.Error(err, "get payment handler failed")
			return err
		}
		//delete payment if it is exist for more than 5 minutes
		if time.Since(payment.CreationTimestamp.Time) > time.Minute*5 {
			if payment.Status.TradeNO != "" {
				status, amount, err := payHandler.GetPaymentDetails(payment.Status.TradeNO)
				if err != nil {
					r.Logger.Error(err, "get payment details failed")
				}
				if status == pay.StatusSuccess {
					r.Logger.Info("payment success, post delete payment cr", "payment", payment, "amount", amount)
				}
				// expire session
				if err = payHandler.ExpireSession(payment.Status.TradeNO); err != nil {
					r.Logger.Error(err, "cancel payment failed")
				}
			}
			if err := r.Delete(ctx, &payment); err != nil {
				return err
			}
		}
	}
	return nil
}

func SyncAccountStatus(ctx context.Context, client client.Client, account *accountv1.Account) error {
	balance, err := crypto.DecryptInt64(*account.Status.EncryptBalance)
	if err != nil {
		return fmt.Errorf("update decrypt balance failed: %v", err)
	}
	deductionBalance, err := crypto.DecryptInt64(*account.Status.EncryptDeductionBalance)
	if err != nil {
		return fmt.Errorf("update decrypt deduction balance failed: %v", err)
	}
	account.Status.Balance = balance
	account.Status.DeductionBalance = deductionBalance
	return client.Status().Update(ctx, account)
}

func initBalance(account *accountv1.Account) (err error) {
	if account.Status.EncryptBalance == nil {
		encryptBalance, err := crypto.EncryptInt64(account.Status.Balance)
		if err != nil {
			return fmt.Errorf("sync encrypt balance failed: %v", err)
		}
		account.Status.EncryptBalance = encryptBalance
	}
	if account.Status.EncryptDeductionBalance == nil {
		encryptDeductionBalance, err := crypto.EncryptInt64(account.Status.DeductionBalance)
		if err != nil {
			return fmt.Errorf("sync encrypt deduction balance failed: %v", err)
		}
		account.Status.EncryptDeductionBalance = encryptDeductionBalance
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AccountReconciler) SetupWithManager(mgr ctrl.Manager, rateOpts controller.Options) error {
	r.Logger = ctrl.Log.WithName("account_controller")
	r.AccountSystemNamespace = env.GetEnvWithDefault(ACCOUNTNAMESPACEENV, DEFAULTACCOUNTNAMESPACE)
	return ctrl.NewControllerManagedBy(mgr).
		For(&userv1.User{}, builder.WithPredicates(predicate.And(OnlyCreatePredicate{}))).
		Watches(&source.Kind{Type: &accountv1.Payment{}}, &handler.EnqueueRequestForObject{}).
		WithOptions(rateOpts).
		Complete(r)
}

func RawParseRechargeConfig() (activities pkgtypes.Activities, discountsteps []int64, discountratios []float64, returnErr error) {
	// local test
	//config, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	//if err != nil {
	//	fmt.Printf("Error building kubeconfig: %v\n", err)
	//	os.Exit(1)
	//}
	config, err := rest.InClusterConfig()
	if err != nil {
		returnErr = fmt.Errorf("get in cluster config failed: %v", err)
		return
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		returnErr = fmt.Errorf("get clientset failed: %v", err)
		return
	}
	configMap, err := clientset.CoreV1().ConfigMaps(SEALOS).Get(context.TODO(), RECHARGEGIFT, metav1.GetOptions{})
	if err != nil {
		returnErr = fmt.Errorf("get configmap failed: %v", err)
		return
	}
	if returnErr = parseConfigList(configMap.Data["steps"], &discountsteps, "steps"); returnErr != nil {
		return
	}

	if returnErr = parseConfigList(configMap.Data["ratios"], &discountratios, "ratios"); returnErr != nil {
		return
	}

	if activityStr := configMap.Data["activities"]; activityStr != "" {
		returnErr = json.Unmarshal([]byte(activityStr), &activities)
	}
	return
}

func parseConfigList(s string, list interface{}, configName string) error {
	for _, v := range strings.Split(s, ",") {
		switch list := list.(type) {
		case *[]int64:
			i, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("%s format error: %v", configName, err)
			}
			*list = append(*list, i)
		case *[]float64:
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("%s format error: %v", configName, err)
			}
			*list = append(*list, f)
		}
	}
	return nil
}

func GetUserOwner(user *userv1.User) string {
	own := user.Annotations[userv1.UserAnnotationOwnerKey]
	if own == "" {
		return user.Name
	}
	return own
}

type NamespaceFilterPredicate struct {
	Namespace string
	predicate.Funcs
}

func (p *NamespaceFilterPredicate) Create(e event.CreateEvent) bool {
	return e.Object.GetNamespace() == p.Namespace
}

func (p *NamespaceFilterPredicate) Delete(e event.DeleteEvent) bool {
	return e.Object.GetNamespace() == p.Namespace
}

func (p *NamespaceFilterPredicate) Update(e event.UpdateEvent) bool {
	return e.ObjectOld.GetNamespace() == p.Namespace
}

func (p *NamespaceFilterPredicate) Generic(e event.GenericEvent) bool {
	return e.Object.GetNamespace() == p.Namespace
}

const BaseUnit = 1_000_000

func (r *AccountReconciler) getAmountWithRates(amount int64, account *accountv1.Account) (anno map[string]string, amt int64, err error) {
	userActivities, err := pkgtypes.ParseUserActivities(account.Annotations)
	if err != nil {
		return nil, 0, fmt.Errorf("parse user activities failed: %w", err)
	}

	rechargeDiscount := pkgtypes.RechargeDiscount{
		DiscountSteps: r.RechargeStep,
		DiscountRates: r.RechargeRatio,
	}
	if len(userActivities) > 0 {
		if activityType, phase, _ := pkgtypes.GetUserActivityDiscount(r.Activities, &userActivities); phase != nil {
			if len(phase.RechargeDiscount.DiscountSteps) > 0 {
				rechargeDiscount.DiscountSteps = phase.RechargeDiscount.DiscountSteps
				rechargeDiscount.DiscountRates = phase.RechargeDiscount.DiscountRates
			}
			rechargeDiscount.SpecialDiscount = phase.RechargeDiscount.SpecialDiscount
			rechargeDiscount = phase.RechargeDiscount
			currentPhase := userActivities[activityType].Phases[userActivities[activityType].CurrentPhase]
			anno = pkgtypes.SetUserPhaseRechargeTimes(account.Annotations, activityType, currentPhase.Name, currentPhase.RechargeNums+1)
		}
	}
	return anno, getAmountWithDiscount(amount, rechargeDiscount), nil
}

func getAmountWithDiscount(amount int64, discount pkgtypes.RechargeDiscount) int64 {
	if discount.SpecialDiscount != nil && discount.SpecialDiscount[amount/BaseUnit] != 0 {
		return amount + discount.SpecialDiscount[amount/BaseUnit]*BaseUnit
	}
	var r float64
	for i, s := range discount.DiscountSteps {
		if amount >= s*BaseUnit {
			r = discount.DiscountRates[i]
		} else {
			break
		}
	}
	return int64(math.Ceil(float64(amount)*r/100)) + amount
}
