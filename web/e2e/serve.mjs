// Runs the Beacon that the browser tests use: the real binary, built with
// the web interface in it, against a database of its own in the Postgres of
// compose.yaml. Playwright starts this script and stops it (see
// playwright.config.ts).
//
// Before: `docker compose up -d --wait postgres dex mailpit`, `npm run build`
// here, and `go build -o bin/beacon ./cmd/beacon` in the repository root.
//
// With --dev it is the Beacon behind the development server (npm run dev):
// the same, except that people reach it at the development server's address,
// which is where a sign-in must send them back to.
import { execFileSync, spawn } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

const root = resolve(import.meta.dirname, "../..");
const binary = process.env.BEACON_BINARY ?? resolve(root, "bin/beacon");
if (!existsSync(binary)) {
  console.error(`${binary} does not exist. Build it first: npm run build, then go build -o bin/beacon ./cmd/beacon in the repository root.`);
  process.exit(1);
}

// A database for this run alone, so that what one run leaves cannot fail
// the next. The name holds only what the clock gives.
const database = `beacon_browser_${Date.now()}`;
const psql = (statement) =>
  execFileSync("docker", ["compose", "exec", "-T", "postgres", "psql", "--username", "beacon", "--dbname", "beacon", "--quiet", "--command", statement], {
    cwd: root,
    stdio: ["ignore", "inherit", "inherit"],
  });
psql(`CREATE DATABASE ${database}`);

let config = resolve(import.meta.dirname, "config");
if (process.argv.includes("--dev")) {
  const yaml = readFileSync(join(config, "beacon.yaml"), "utf8").replace("public_url: http://127.0.0.1:18080", "public_url: http://127.0.0.1:5173");
  config = mkdtempSync(join(tmpdir(), "beacon-dev-"));
  writeFileSync(join(config, "beacon.yaml"), yaml);
}

const port = process.env.BEACON_POSTGRES_PORT ?? "5432";
const beacon = spawn(binary, ["serve", "--config", config, "--listen-address", "127.0.0.1:18080", "--log-format", "text"], {
  stdio: "inherit",
  env: {
    PATH: process.env.PATH,
    BEACON_DATABASE_URL: `postgres://beacon:beacon@127.0.0.1:${port}/${database}?sslmode=disable`,
    // The values the tests and the services of compose.yaml share.
    BEACON_SOURCE_TOKEN: "browser-test-source-token",
    BEACON_OIDC_CLIENT_SECRET: "beacon-local-development-client-secret",
    BEACON_SMTP_PASSWORD: "beacon-local-development-password",
    BEACON_DISCORD_BOT_TOKEN: "browser-test-discord-bot-token",
    // Nothing listens there, so no request of the tests reaches Discord.
    BEACON_DISCORD_API_URL: "http://127.0.0.1:9",
  },
});

let stopping = false;
function stop() {
  if (!stopping) {
    stopping = true;
    beacon.kill("SIGTERM");
  }
}
process.on("SIGTERM", stop);
process.on("SIGINT", stop);

beacon.on("exit", (code) => {
  try {
    psql(`DROP DATABASE IF EXISTS ${database} WITH (FORCE)`);
  } catch {
    // The database stays; it has a name of its own and is in nobody's way.
  }
  process.exit(stopping ? 0 : (code ?? 1));
});
