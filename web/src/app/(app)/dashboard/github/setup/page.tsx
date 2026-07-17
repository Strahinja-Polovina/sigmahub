import Link from "next/link";
import { redirect } from "next/navigation";
import { getActiveOrgId } from "@/server/active-org";
import { linkInstallation } from "@/server/actions/git";
import { isInstallationId, parseInstallState } from "@/lib/github-app";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";

// GitHub App post-install callback (SIGMA-55). The App's Setup URL points
// here; GitHub arrives with ?installation_id=…&state=… where state is the
// target we encoded into the install link. An existing connection is linked
// immediately; a new-connection flow bounces back to the project page with
// the installation id in the query so the connect dialog picks it up. State
// is validated strictly — a forged value renders an error, it never links.
export default async function GitHubSetupPage({
  searchParams,
}: {
  searchParams: Promise<Record<string, string | string[] | undefined>>;
}) {
  const params = await searchParams;
  const orgId = await getActiveOrgId();
  if (!orgId) redirect("/login");

  const installationId = typeof params.installation_id === "string" ? params.installation_id : undefined;
  const target = parseInstallState(typeof params.state === "string" ? params.state : undefined);

  let error: string;
  if (!isInstallationId(installationId)) {
    error = "GitHub did not send a valid installation id.";
  } else if (!target) {
    error = "The install link's state is missing or malformed, so the installation can't be routed to a project.";
  } else if (target.kind === "project") {
    redirect(
      `/dashboard/projects/${target.projectId}?installation_id=${installationId}`
    );
  } else {
    try {
      await linkInstallation({
        orgId,
        projectId: target.projectId,
        connectionId: target.connectionId,
        installationId,
      });
    } catch (err) {
      error = err instanceof Error ? err.message : "Linking the installation failed.";
      return <SetupError message={error} />;
    }
    redirect(`/dashboard/projects/${target.projectId}`);
  }
  return <SetupError message={error} />;
}

function SetupError({ message }: { message: string }) {
  return (
    <div className="p-6">
      <Alert variant="destructive">
        <AlertTitle>GitHub App installation not linked</AlertTitle>
        <AlertDescription>
          {message} You can retry from the project&apos;s Git panel, or paste an
          access token instead.{" "}
          <Link href="/dashboard/projects" className="underline">
            Back to projects
          </Link>
        </AlertDescription>
      </Alert>
    </div>
  );
}
