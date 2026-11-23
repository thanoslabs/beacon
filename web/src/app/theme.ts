import { useEffect, useState } from "react";

export type Theme = "system" | "light" | "dark";

const key = "beacon.theme";

function stored(): Theme {
  try {
    const value = localStorage.getItem(key);
    return value === "light" || value === "dark" ? value : "system";
  } catch {
    return "system";
  }
}

/**
 * The theme a person chose, kept in this browser. "system" follows the
 * device, which is the style sheet's default; public/theme.js applies a
 * choice before the first paint.
 */
export function useTheme(): [Theme, (theme: Theme) => void] {
  const [theme, setTheme] = useState<Theme>(stored);
  useEffect(() => {
    if (theme === "system") {
      delete document.documentElement.dataset.theme;
    } else {
      document.documentElement.dataset.theme = theme;
    }
    try {
      if (theme === "system") {
        localStorage.removeItem(key);
      } else {
        localStorage.setItem(key, theme);
      }
    } catch {
      // The choice then lasts until the page is closed.
    }
  }, [theme]);
  return [theme, setTheme];
}
