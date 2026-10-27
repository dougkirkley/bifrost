import { NoPermissionView } from "@/components/noPermissionView";
import { RbacOperation, RbacResource, useRbac } from "@enterprise/lib";
import { createFileRoute } from "@tanstack/react-router";
import RelaysPage from "./page";

// Relays are configured with the same permission as providers.
function RouteComponent() {
	const hasAccess = useRbac(RbacResource.ModelProvider, RbacOperation.View);
	if (!hasAccess) {
		return <NoPermissionView entity="relays" />;
	}
	return <RelaysPage />;
}

export const Route = createFileRoute("/workspace/relays")({
	component: RouteComponent,
});