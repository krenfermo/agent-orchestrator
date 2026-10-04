import { Building2, Check } from "lucide-react";
import { useTranslation } from "react-i18next";
import { useQueryClient } from "@tanstack/react-query";
import {
	DropdownMenu,
	DropdownMenuContent,
	DropdownMenuItem,
	DropdownMenuTrigger,
} from "./ui/dropdown-menu";
import { Tooltip, TooltipContent, TooltipTrigger } from "./ui/tooltip";
import { cn } from "../lib/utils";
import { projectsListQueryKey } from "../hooks/useProjectsList";
import { useTenantStore } from "../stores/tenant-store";

// P4-C: the organization switcher.
//
// It renders NOTHING unless the account belongs to more than one organization,
// which is the whole design constraint: the single-organization installation --
// still the overwhelmingly common one, and every existing install on the day
// P4-C ships -- must look exactly as it did before, with no new concept on
// screen. There is no "switch to your only organization" affordance, because
// there is nothing to switch to.
//
// Switching is a VIEW change, not an authorization change. The daemon already
// refuses everything outside the organizations this account belongs to; what
// this control does is choose which of them is currently on screen.

const ROW_CLASS =
	"h-9 gap-2.5 rounded-lg px-2.5 text-sm font-medium text-muted-foreground transition-[background-color,color] hover:bg-interactive-hover hover:text-foreground";

export function TenantSwitcher({ tabIndex }: { tabIndex: number }) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const tenants = useTenantStore((s) => s.tenants);
	const currentTenantId = useTenantStore((s) => s.currentTenantId);
	const setCurrentTenant = useTenantStore((s) => s.setCurrentTenant);

	if (tenants.length <= 1) return null;

	const current = tenants.find((tenant) => tenant.id === currentTenantId) ?? tenants[0];

	const select = (id: string) => {
		if (id === currentTenantId) return;
		setCurrentTenant(id);
		// Drop every organization's project list on the way out. The list is
		// keyed by organization, so the new one cannot render the old one's
		// rows -- but a refetch on switch is what keeps a list that was loaded
		// before somebody's access changed from being served back to them.
		void queryClient.invalidateQueries({ queryKey: projectsListQueryKey });
	};

	return (
		<DropdownMenu>
			<Tooltip>
				<TooltipTrigger asChild>
					<DropdownMenuTrigger asChild>
						<button
							aria-label={t("shell.switchOrganization")}
							className={cn(
								ROW_CLASS,
								"flex w-full items-center text-left [&_svg]:size-icon-md [&_svg]:shrink-0",
							)}
							tabIndex={tabIndex}
							type="button"
						>
							<Building2 aria-hidden="true" />
							<span className="min-w-0 flex-1 truncate tracking-tight">{current.name}</span>
						</button>
					</DropdownMenuTrigger>
				</TooltipTrigger>
				<TooltipContent side="right" hidden={tabIndex !== -1}>
					{t("shell.switchOrganization")}
				</TooltipContent>
			</Tooltip>
			<DropdownMenuContent side="top" align="start" className="min-w-52">
				{tenants.map((tenant) => (
					<DropdownMenuItem key={tenant.id} onSelect={() => select(tenant.id)}>
						{tenant.id === current.id ? (
							<Check aria-hidden="true" />
						) : (
							<span aria-hidden="true" className="size-icon-md" />
						)}
						<span className="min-w-0 flex-1 truncate">{tenant.name}</span>
					</DropdownMenuItem>
				))}
			</DropdownMenuContent>
		</DropdownMenu>
	);
}
