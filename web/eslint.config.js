import js from "@eslint/js";
import reactHooks from "eslint-plugin-react-hooks";
import globals from "globals";
import tseslint from "typescript-eslint";

export default tseslint.config(
  { ignores: ["node_modules", "src/api/schema.d.ts", "playwright-report", "test-results"] },
  js.configs.recommended,
  ...tseslint.configs.strictTypeChecked,
  reactHooks.configs.flat.recommended,
  {
    languageOptions: {
      globals: globals.browser,
      parserOptions: { projectService: true, tsconfigRootDir: import.meta.dirname },
    },
    rules: {
      // Alert text is written by whoever can send an alert. It is only ever
      // rendered as text; these keep it from becoming markup or a script.
      "no-restricted-syntax": [
        "error",
        { selector: "JSXAttribute[name.name='dangerouslySetInnerHTML']", message: "Never render HTML from data: alert text is attacker-controlled." },
        { selector: "MemberExpression[property.name=/^(innerHTML|outerHTML)$/]", message: "Never write HTML from data: alert text is attacker-controlled." },
        { selector: "CallExpression[callee.property.name=/^(insertAdjacentHTML|write|writeln)$/][callee.object.name=/^(document|el|element|node)$/]", message: "Never write HTML from data." },
      ],
      "no-eval": "error",
      "no-implied-eval": "off",
      "@typescript-eslint/no-implied-eval": "error",
      // Numbers and booleans read well in template strings.
      "@typescript-eslint/restrict-template-expressions": ["error", { allowNumber: true, allowBoolean: true }],
      "@typescript-eslint/no-confusing-void-expression": ["error", { ignoreArrowShorthand: true }],
    },
  },
  {
    files: ["e2e/**", "*.config.ts", "*.config.js", "**/*.mjs"],
    languageOptions: { globals: globals.node },
  },
  {
    // Plain scripts are in no TypeScript project.
    files: ["**/*.js", "**/*.mjs"],
    ...tseslint.configs.disableTypeChecked,
  },
  {
    // The checks above are for the app; in tests a non-null assertion on
    // what the test itself set up is clear enough.
    files: ["src/**/*.test.{ts,tsx}", "src/test/**", "e2e/**"],
    rules: { "@typescript-eslint/no-non-null-assertion": "off" },
  },
);
