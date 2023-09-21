import { Icon, IconProps } from '@chakra-ui/react';

export function NetworkIcon(props: IconProps) {
  return (
    <Icon
      width="16"
      height="16"
      viewBox="0 0 16 16"
      fill="none"
      xmlns="http://www.w3.org/2000/svg"
      {...props}
    >
      <path
        d="M5.29739 2.28449C5.44471 1.68914 6.22675 1.67132 6.41898 2.21967L8.49799 11.1827L9.80448 6.55581C9.9482 6.04505 10.5901 5.96061 10.8588 6.38597L12.2001 9.13356H14.472C14.8033 9.13356 15.0718 9.40249 15.0718 9.73416C15.0718 10.0846 14.7881 10.3687 14.438 10.3687H11.8395C11.644 10.3689 11.4613 10.2673 11.3516 10.0976L10.5154 8.39063L9.00668 13.7352C8.84278 14.315 8.07843 14.3193 7.89048 13.7784L5.84782 4.97473L4.62438 9.90676C4.56302 10.154 4.36278 10.3361 4.12052 10.365L1.52053 10.3679C1.19356 10.3683 0.928223 10.103 0.928223 9.77548V9.7108C0.928223 9.39199 1.18637 9.13342 1.50477 9.13342H3.59787L5.29739 2.28449Z"
        fill="#24282C"
      />
    </Icon>
  );
}