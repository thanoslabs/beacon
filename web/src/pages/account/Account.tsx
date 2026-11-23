import { useSession } from "../../app/session";
import { PageHead, Time, useTitle } from "../../app/ui";
import { ChatAccounts } from "./ChatAccounts";

/** Who is signed in. */
export function Account() {
  useTitle("Account");
  const me = useSession();
  return (
    <div className="page">
      <PageHead title="Account" />
      <section className="card" aria-labelledby="account-you">
        <div className="card-body stack-small">
          <h2 id="account-you">Signed in as</h2>
          <dl className="facts">
            <dt>Name</dt>
            <dd>{me.display_name}</dd>
            {me.email !== undefined && (
              <>
                <dt>Email</dt>
                <dd>{me.email}</dd>
              </>
            )}
            <dt>Role</dt>
            <dd>{me.role === "admin" ? "Admin" : "Member"}</dd>
            {me.session_expires_at !== undefined && (
              <>
                <dt>Signed in until</dt>
                <dd>
                  <Time value={me.session_expires_at} />
                </dd>
              </>
            )}
          </dl>
          <p className="hint">
            Your name, role and teams come from your organisation’s identity provider and are updated each time you
            sign in.
          </p>
        </div>
      </section>
      <section className="card" aria-labelledby="account-chat">
        <div className="card-body stack-small">
          <h2 id="account-chat">Chat accounts</h2>
          <p className="muted">Beacon needs to know which account in Slack or Discord is yours, to page you there and to know that it is you who acknowledges.</p>
          <ChatAccounts />
        </div>
      </section>
    </div>
  );
}
