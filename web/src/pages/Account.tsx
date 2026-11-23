import { useSession } from "../app/session";
import { PageHead, Time, useTitle } from "../app/ui";

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
    </div>
  );
}
