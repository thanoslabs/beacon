import { useEffect, useRef, useState } from "react";
import { Link, NavLink, NavigationType, Outlet, useLocation, useNavigationType } from "react-router";
import { ErrorNotice } from "./ui";
import { useSession, useSignOut } from "./session";
import { useTheme, type Theme } from "./theme";

interface NavItem {
  to: string;
  label: string;
}

interface NavSection {
  heading: string;
  items: NavItem[];
  adminOnly?: boolean;
}

const sections: NavSection[] = [
  {
    heading: "Respond",
    items: [
      { to: "/alerts", label: "Alerts" },
      { to: "/on-call", label: "On call" },
      { to: "/silences", label: "Silences" },
    ],
  },
  {
    heading: "Configuration",
    items: [
      { to: "/escalation-policies", label: "Escalation policies" },
      { to: "/routing", label: "Routing" },
      { to: "/teams", label: "Teams" },
      { to: "/integrations", label: "Sources and integrations" },
    ],
  },
  { heading: "You", items: [{ to: "/account", label: "Account" }] },
  // Offered to admins only. The API refuses a member whatever the app shows.
  { heading: "Administration", adminOnly: true, items: [{ to: "/tokens", label: "API tokens" }] },
];

/** The frame around every page behind the sign-in. */
export function Layout() {
  const me = useSession();
  const signOut = useSignOut();
  const [theme, setTheme] = useTheme();
  const [open, setOpen] = useState(false);
  const [signOutError, setSignOutError] = useState<unknown>(null);
  const location = useLocation();
  const navigation = useNavigationType();
  const main = useRef<HTMLElement>(null);
  const shown = useRef(location.pathname);

  // A page that a person went to starts at its top, with the menu closed and
  // the keyboard at the page, as after a page load. A redirect is not a
  // page they went to: the keyboard stays where it was.
  useEffect(() => {
    if (shown.current === location.pathname) {
      return;
    }
    shown.current = location.pathname;
    setOpen(false);
    if (navigation !== NavigationType.Replace) {
      main.current?.focus();
      window.scrollTo(0, 0);
    }
  }, [location.pathname, navigation]);

  return (
    <div className="frame">
      <a className="skip-link" href="#main">
        Skip to the page
      </a>
      <header className="sidebar" data-open={open}>
        <Link className="brand" to="/">
          <img src="/favicon.svg" alt="" />
          Beacon
        </Link>
        <button
          type="button"
          className="button menu-button"
          aria-expanded={open}
          aria-controls="navigation"
          onClick={() => setOpen((was) => !was)}
        >
          Menu
        </button>
        <nav className="nav" id="navigation" aria-label="Main">
          {sections
            .filter((section) => !section.adminOnly || me.role === "admin")
            .map((section) => (
            <div key={section.heading}>
              <h2 className="nav-heading">{section.heading}</h2>
              <ul>
                {section.items.map((item) => (
                  <li key={item.to}>
                    <NavLink to={item.to}>{item.label}</NavLink>
                  </li>
                ))}
              </ul>
            </div>
          ))}
        </nav>
        <div className="sidebar-foot">
          <p className="who">
            <strong>{me.display_name}</strong>
            <br />
            <span className="muted">{me.role === "admin" ? "Admin" : "Member"}</span>
          </p>
          <div className="field">
            <label htmlFor="theme">Theme</label>
            <select id="theme" className="input" value={theme} onChange={(event) => setTheme(event.target.value as Theme)}>
              <option value="system">Same as the device</option>
              <option value="light">Light</option>
              <option value="dark">Dark</option>
            </select>
          </div>
          {signOutError !== null && <ErrorNotice error={signOutError} />}
          <button
            type="button"
            className="button"
            onClick={() => {
              setSignOutError(null);
              signOut().catch(setSignOutError);
            }}
          >
            Sign out
          </button>
        </div>
      </header>
      <main className="main" id="main" tabIndex={-1} ref={main}>
        <Outlet />
      </main>
    </div>
  );
}
