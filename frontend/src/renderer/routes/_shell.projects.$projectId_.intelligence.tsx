import { createFileRoute } from "@tanstack/react-router";
import { ProjectIntelligenceView } from "../components/intelligence/ProjectIntelligenceView";

export const Route = createFileRoute("/_shell/projects/$projectId_/intelligence")({
	component: ProjectIntelligenceRoute,
});

function ProjectIntelligenceRoute() {
	const { projectId } = Route.useParams();
	return <ProjectIntelligenceView projectId={projectId} />;
}
