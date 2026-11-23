import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { Navigate, Route, Routes } from "react-router";
import { setSessionEndedHandler } from "../api/client";
import { Account } from "../pages/account/Account";
import { Tokens } from "../pages/account/Tokens";
import { AlertDetail } from "../pages/alerts/AlertDetail";
import { AlertList } from "../pages/alerts/AlertList";
import { Connections } from "../pages/configuration/Connections";
import { Policies } from "../pages/configuration/Policies";
import { RoutingPage } from "../pages/configuration/RoutingPage";
import { Teams } from "../pages/configuration/Teams";
import { Login } from "../pages/Login";
import { OnCall } from "../pages/schedules/OnCall";
import { SchedulePage } from "../pages/schedules/SchedulePage";
import { NewSilence } from "../pages/silences/NewSilence";
import { SilenceList } from "../pages/silences/SilenceList";
import { NotFound } from "../pages/NotFound";
import { Layout } from "./Layout";
import { SessionProvider, forgetSession, useMeQuery } from "./session";
import { ErrorNotice, Loading } from "./ui";

/** The first page a signed-in person sees. */
export const homePath = "/alerts";

/**
 * The app: the sign-in page for a stranger, the pages for whoever is signed
 * in. The app never decides who that is; it asks the API, and shows the
 * sign-in page again as soon as the API says nobody is.
 */
export function App() {
  const queryClient = useQueryClient();
  const me = useMeQuery();
  const [sessionEnded, setSessionEnded] = useState(false);

  useEffect(() => {
    setSessionEndedHandler(() => {
      setSessionEnded(true);
      forgetSession(queryClient);
    });
    return () => setSessionEndedHandler(() => undefined);
  }, [queryClient]);

  if (me.isPending) {
    return (
      <main className="sign-in">
        <Loading label="Opening Beacon" />
      </main>
    );
  }
  if (me.isError) {
    return (
      <main className="sign-in">
        <ErrorNotice error={me.error} onRetry={() => void me.refetch()} />
      </main>
    );
  }
  if (me.data === null) {
    return <Login sessionEnded={sessionEnded} />;
  }
  return (
    <SessionProvider value={me.data}>
      <Routes>
        <Route element={<Layout />}>
          <Route index element={<Navigate to={homePath} replace />} />
          <Route path="alerts" element={<AlertList />} />
          <Route path="alerts/:id" element={<AlertDetail />} />
          <Route path="on-call" element={<OnCall />} />
          <Route path="schedules/:name" element={<SchedulePage />} />
          <Route path="silences" element={<SilenceList />} />
          <Route path="silences/new" element={<NewSilence />} />
          <Route path="escalation-policies" element={<Policies />} />
          <Route path="routing" element={<RoutingPage />} />
          <Route path="teams" element={<Teams />} />
          <Route path="integrations" element={<Connections />} />
          <Route path="account" element={<Account />} />
          <Route path="tokens" element={<Tokens />} />
          <Route path="*" element={<NotFound />} />
        </Route>
      </Routes>
    </SessionProvider>
  );
}
