// Applies the theme a person chose before the page is drawn, so that it does
// not flash in the other one. It is a file of its own because the
// Content-Security-Policy allows no inline script. Without a choice the
// style sheet follows the system's setting.
try {
  var theme = localStorage.getItem("beacon.theme");
  if (theme === "light" || theme === "dark") {
    document.documentElement.dataset.theme = theme;
  }
} catch {
  // Storage is not available; the system's setting applies.
}
