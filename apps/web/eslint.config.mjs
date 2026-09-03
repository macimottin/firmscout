import nextConfig from "eslint-config-next";

const eslintConfig = [
  ...nextConfig,
  {
    ignores: [
      "node_modules/**",
      ".next/**",
      "out/**",
      "next-env.d.ts",
      "coverage/**",
    ],
  },
];

export default eslintConfig;
