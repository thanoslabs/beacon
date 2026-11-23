import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type SyntheticEvent } from "react";
import { api, unwrap } from "../api/client";
import { meKey, rememberReturnTo } from "../app/session";
import { ErrorNotice, Loading, useTitle } from "../app/ui";

/**
 * The sign-in page. It is shown in place of whatever page was asked for, so
 * the address stays, and the person arrives there once they are signed in.
 */
export function Login({ sessionEnded }: { sessionEnded: boolean }) {
  useTitle("Sign in");
  const methods = useQuery({
    queryKey: ["auth", "methods"],
    queryFn: ({ signal }) => unwrap(api.GET("/auth/methods", { signal })),
  });

  return (
    <main className="sign-in">
      <div className="card">
        <div className="card-body stack">
          <p className="brand">
            <img src="/favicon.svg" alt="" />
            Beacon
          </p>
          <h1>Sign in</h1>
          {sessionEnded && (
            <p className="notice notice-info" role="status">
              Your session has ended. Sign in again to continue where you were.
            </p>
          )}
          {methods.isPending && <Loading label="Finding out how to sign in" />}
          {methods.isError && <ErrorNotice error={methods.error} onRetry={() => void methods.refetch()} />}
          {methods.data && <Methods oidc={methods.data.oidc} bootstrapAdmin={methods.data.bootstrap_admin} />}
        </div>
      </div>
    </main>
  );
}

function Methods({ oidc, bootstrapAdmin }: { oidc: boolean; bootstrapAdmin: boolean }) {
  if (!oidc && !bootstrapAdmin) {
    return (
      <p className="notice notice-info">
        Nobody can sign in yet: this Beacon has no <code>auth</code> block in its configuration files. An administrator
        adds an identity provider or the bootstrap admin there.
      </p>
    );
  }
  return (
    <>
      {oidc && (
        <a
          className="button button-primary"
          href="/auth/login"
          onClick={() => rememberReturnTo(window.location.pathname + window.location.search)}
        >
          Sign in with your identity provider
        </a>
      )}
      {oidc && bootstrapAdmin && <p className="divider">or</p>}
      {bootstrapAdmin && <BootstrapForm />}
    </>
  );
}

/** The bootstrap admin's password, for an install without a provider yet. */
function BootstrapForm() {
  const queryClient = useQueryClient();
  const [password, setPassword] = useState("");
  const signIn = useMutation({
    mutationFn: (value: string) => unwrap(api.POST("/auth/bootstrap", { body: { password: value } })),
    onSuccess: (me) => queryClient.setQueryData(meKey, me),
  });

  function submit(event: SyntheticEvent) {
    event.preventDefault();
    signIn.mutate(password);
  }

  return (
    <form className="stack" onSubmit={submit}>
      <div className="field">
        <label htmlFor="bootstrap-password">Bootstrap admin password</label>
        <input
          id="bootstrap-password"
          className="input"
          type="password"
          autoComplete="current-password"
          required
          value={password}
          onChange={(event) => setPassword(event.target.value)}
          aria-describedby="bootstrap-hint"
        />
        <p className="hint" id="bootstrap-hint">
          The built-in admin account, for an install that has no identity provider yet.
        </p>
      </div>
      {signIn.isError && <ErrorNotice error={signIn.error} />}
      <button type="submit" className="button" disabled={signIn.isPending}>
        {signIn.isPending ? "Signing in…" : "Sign in as the bootstrap admin"}
      </button>
    </form>
  );
}
