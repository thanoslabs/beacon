import { Link } from "react-router";
import { PageHead, useTitle } from "../app/ui";

export function NotFound() {
  useTitle("Page not found");
  return (
    <div className="page">
      <PageHead title="Page not found" />
      <p>
        There is no page at this address. <Link to="/">Go to the first page</Link>.
      </p>
    </div>
  );
}
