import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router";
import { ApiError } from "./api/client";
import { App } from "./app/App";
import { takeReturnTo } from "./app/session";
import "./styles.css";

// After a sign-in the identity provider sends the browser to the first
// page. If the person had asked for another page, that is where they go.
if (window.location.pathname === "/") {
  const returnTo = takeReturnTo();
  if (returnTo !== null) {
    window.history.replaceState(null, "", returnTo);
  }
}

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // What the app shows is a copy of what Beacon knows: ask again when
      // the person comes back to the tab.
      refetchOnWindowFocus: true,
      staleTime: 5_000,
      // A request that the API refused will be refused again; one that
      // never arrived is worth another try.
      retry: (failures, error) => failures < 2 && (!(error instanceof ApiError) || error.status === 0 || error.status >= 500),
    },
  },
});

const root = document.getElementById("root");
if (root === null) {
  throw new Error("the page has no root element");
}
createRoot(root).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
);
