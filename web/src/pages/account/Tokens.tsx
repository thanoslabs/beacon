import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useId, useState, type SyntheticEvent } from "react";
import { ApiError, api, unwrap, type Schemas } from "../../api/client";
import { useIsAdmin } from "../../app/session";
import { Dialog, Empty, ErrorNotice, Loading, PageHead, Time, useTitle } from "../../app/ui";

type Token = Schemas["Token"];
type CreatedToken = Schemas["CreatedToken"];

const tokensKey = ["tokens"] as const;
const pages = 5;

const lifetimes = [
  { value: "30d", label: "30 days" },
  { value: "90d", label: "90 days" },
  { value: "52w", label: "1 year" },
  { value: "", label: "It does not expire" },
];

/**
 * API tokens, for admins: a token can carry any role, so managing them is
 * theirs alone. A member who comes here is told so, and nothing is asked
 * of the API, which would refuse.
 */
export function Tokens() {
  useTitle("API tokens");
  const admin = useIsAdmin();
  if (!admin) {
    return (
      <div className="page">
        <PageHead title="API tokens" />
        <p className="notice notice-info">API tokens are managed by admins. Ask an admin of Beacon for a token.</p>
      </div>
    );
  }
  return <TokenList />;
}

function TokenList() {
  const tokens = useQuery({
    queryKey: tokensKey,
    queryFn: async ({ signal }) => {
      const all: Token[] = [];
      let cursor: string | undefined;
      for (let page = 0; page < pages; page++) {
        const answer = await unwrap(api.GET("/api/v1/tokens", { params: { query: { limit: 200, cursor } }, signal }));
        all.push(...answer.tokens);
        cursor = answer.next_cursor;
        if (cursor === undefined) {
          break;
        }
      }
      return all;
    },
  });
  const [creating, setCreating] = useState(false);
  // The secret of a token that was just created. It is in the page's
  // memory only, and gone when the notice is closed or the page is left.
  const [created, setCreated] = useState<CreatedToken | null>(null);
  const [revoking, setRevoking] = useState<Token | null>(null);

  return (
    <div className="page">
      <PageHead title="API tokens">
        <button type="button" className="button button-primary" onClick={() => setCreating(true)}>
          New token
        </button>
      </PageHead>
      <p className="muted">A token lets a script or another system use the API with a role of its own. It is not tied to the person who created it, and keeps working when they leave.</p>
      {created !== null && <Secret token={created} onDone={() => setCreated(null)} />}
      {tokens.data === undefined && tokens.isPending && <Loading label="Loading tokens" />}
      {tokens.isError && <ErrorNotice error={tokens.error} onRetry={() => void tokens.refetch()} />}
      {tokens.data?.length === 0 && <Empty title="No API tokens">Nothing but people can use the API.</Empty>}
      {tokens.data !== undefined && tokens.data.length > 0 && (
        <ul className="silence-list" aria-label="API tokens">
          {tokens.data.map((token) => (
            <TokenRow key={token.id} token={token} onRevoke={() => setRevoking(token)} />
          ))}
        </ul>
      )}
      {creating && (
        <NewToken
          onClose={() => setCreating(false)}
          onCreated={(token) => {
            setCreating(false);
            setCreated(token);
          }}
        />
      )}
      {revoking !== null && <RevokeToken token={revoking} onClose={() => setRevoking(null)} />}
    </div>
  );
}

function TokenRow({ token, onRevoke }: { token: Token; onRevoke: () => void }) {
  const revoked = token.revoked_at !== undefined;
  return (
    <li className="card silence-row">
      <div className="stack-small">
        <p className="row">
          <strong>{token.name}</strong>
          <span className="badge">{token.role === "admin" ? "Admin" : "Member"}</span>
          {revoked && <span className="badge status-firing">Revoked</span>}
        </p>
        <p className="muted">
          Created <Time value={token.created_at} />
          {token.created_by !== undefined && ` by ${token.created_by}`}
          {" · "}
          {token.expires_at !== undefined ? (
            <>
              expires <Time value={token.expires_at} />
            </>
          ) : (
            "does not expire"
          )}
          {" · "}
          {token.last_used_at !== undefined ? (
            <>
              last used <Time value={token.last_used_at} />
            </>
          ) : (
            "never used"
          )}
        </p>
      </div>
      {!revoked && (
        <button type="button" className="button" aria-label={`Revoke the token ${token.name}`} onClick={onRevoke}>
          Revoke
        </button>
      )}
    </li>
  );
}

function Secret({ token, onDone }: { token: CreatedToken; onDone: () => void }) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="notice notice-ok" role="status">
      <p>
        <strong>The token {token.name} was created.</strong> Copy it now: Beacon keeps only a hash of it, and it cannot be shown again.
      </p>
      <p>
        <code className="secret">{token.token}</code>
      </p>
      <div className="row">
        <button
          type="button"
          className="button"
          onClick={() => {
            navigator.clipboard.writeText(token.token).then(
              () => setCopied(true),
              () => setCopied(false),
            );
          }}
        >
          {copied ? "Copied" : "Copy"}
        </button>
        <button type="button" className="button" onClick={onDone}>
          I have stored it
        </button>
      </div>
    </div>
  );
}

function NewToken({ onClose, onCreated }: { onClose: () => void; onCreated: (token: CreatedToken) => void }) {
  const queryClient = useQueryClient();
  const ids = { name: useId(), role: useId(), lifetime: useId() };
  const [name, setName] = useState("");
  const [role, setRole] = useState<Token["role"]>("member");
  const [lifetime, setLifetime] = useState("90d");
  const create = useMutation({
    mutationFn: () => unwrap(api.POST("/api/v1/tokens", { body: { name: name.trim(), role, expires_in: lifetime === "" ? undefined : lifetime } })),
    onSuccess: async (token) => {
      await queryClient.invalidateQueries({ queryKey: tokensKey });
      onCreated(token);
    },
  });
  const fault = create.error instanceof ApiError ? create.error.field : undefined;

  function submit(event: SyntheticEvent) {
    event.preventDefault();
    create.mutate();
  }

  return (
    <Dialog title="New API token" onClose={onClose}>
      <form className="stack" onSubmit={submit}>
        <div className="field">
          <label htmlFor={ids.name}>What it is for</label>
          <input id={ids.name} className="input" required maxLength={100} value={name} onChange={(event) => setName(event.target.value)} aria-invalid={fault === "name"} />
        </div>
        <div className="field">
          <label htmlFor={ids.role}>Role</label>
          <select id={ids.role} className="input" value={role} onChange={(event) => setRole(event.target.value as Token["role"])} aria-invalid={fault === "role"}>
            <option value="member">Member: reads everything, acts on alerts, silences and overrides</option>
            <option value="admin">Admin: also manages tokens and reloads the configuration</option>
          </select>
        </div>
        <div className="field">
          <label htmlFor={ids.lifetime}>Valid for</label>
          <select id={ids.lifetime} className="input" value={lifetime} onChange={(event) => setLifetime(event.target.value)} aria-invalid={fault === "expires_in"}>
            {lifetimes.map((choice) => (
              <option key={choice.value} value={choice.value}>
                {choice.label}
              </option>
            ))}
          </select>
        </div>
        {create.isError && <ErrorNotice error={create.error} />}
        <div className="row">
          <button type="submit" className="button button-primary" disabled={create.isPending}>
            Create the token
          </button>
          <button type="button" className="button" onClick={onClose}>
            Cancel
          </button>
        </div>
      </form>
    </Dialog>
  );
}

function RevokeToken({ token, onClose }: { token: Token; onClose: () => void }) {
  const queryClient = useQueryClient();
  const revoke = useMutation({
    mutationFn: () => unwrap(api.DELETE("/api/v1/tokens/{id}", { params: { path: { id: token.id } } })),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: tokensKey });
      onClose();
    },
  });
  return (
    <Dialog title={`Revoke the token ${token.name}?`} onClose={onClose}>
      <p>It stops working at once, and whatever uses it is refused. This cannot be undone; create another token to replace it.</p>
      {revoke.isError && <ErrorNotice error={revoke.error} />}
      <div className="row">
        <button type="button" className="button button-primary" disabled={revoke.isPending} onClick={() => revoke.mutate()}>
          Revoke the token
        </button>
        <button type="button" className="button" onClick={onClose}>
          Keep it
        </button>
      </div>
    </Dialog>
  );
}
