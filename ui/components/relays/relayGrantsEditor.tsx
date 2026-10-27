import { RelaySelector } from "@/components/entitySelectors/relaySelector";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { Info, Trash2 } from "lucide-react";
import type { ReactNode } from "react";

const DEFAULT_TOOLTIP = (
	<p>
		Let this key call relays at <span className="font-medium">/relay/&lt;name&gt;/...</span>. Each call's cost is charged to the key's
		budgets. With neither option set the key may call none.
	</p>
);

interface RelayGrantsEditorProps {
	allowAll: boolean;
	onAllowAllChange: (allowAll: boolean) => void;
	/** Granted endpoint names; endpoints are keyed by name, so the name is the grant. */
	value: string[];
	onChange: (endpointNames: string[]) => void;
	label?: string;
	/** Overridable so a holder other than a virtual key (an access profile, a project) can say what the grant means. */
	tooltip?: ReactNode;
	testIdPrefix?: string;
}

/**
 * Grants access to relays: every endpoint at once, or endpoints one by one. Mirrors
 * the agent and MCP editors beside it: a picker on top, removable rows beneath.
 */
export function RelayGrantsEditor({
	allowAll,
	onAllowAllChange,
	value,
	onChange,
	label = "Relay access",
	tooltip = DEFAULT_TOOLTIP,
	testIdPrefix = "vk-relays",
}: RelayGrantsEditorProps) {
	const add = (option: { value: string }) => {
		if (value.includes(option.value)) return;
		onChange([...value, option.value]);
	};
	const remove = (name: string) => onChange(value.filter((v) => v !== name));

	return (
		<div className="mt-6 space-y-2" data-testid={`${testIdPrefix}-editor`}>
			<div className="flex items-center gap-2">
				<Label className="text-sm font-medium">{label}</Label>
				<TooltipProvider>
					<Tooltip>
						<TooltipTrigger asChild>
							<span>
								<Info className="text-muted-foreground h-3 w-3" />
							</span>
						</TooltipTrigger>
						<TooltipContent className="max-w-xs">{tooltip}</TooltipContent>
					</Tooltip>
				</TooltipProvider>
			</div>

			<div className="flex items-center justify-between rounded-md border px-3 py-2">
				<div>
					<div className="text-sm">Allow all relays</div>
					<div className="text-muted-foreground text-xs">Includes relays added later.</div>
				</div>
				<Switch
					aria-label="Allow all relays"
					checked={allowAll}
					onCheckedChange={onAllowAllChange}
					data-testid={`${testIdPrefix}-allow-all`}
				/>
			</div>

			{!allowAll && (
				<>
					<RelaySelector mode="add" fullWidth placeholder="Select a relay to add" onSelect={add} excludeIds={value} />
					{value.length > 0 && (
						<div className="divide-y overflow-hidden rounded-md border">
							{value.map((name) => (
								<div key={name} className="flex items-center justify-between gap-2 px-3 py-2" data-testid={`${testIdPrefix}-grant-${name}`}>
									<div className="flex min-w-0 flex-col">
										<span className="truncate text-sm font-medium">{name}</span>
										<span className="text-muted-foreground truncate text-xs">/relay/{name}</span>
									</div>
									<Button type="button" variant="ghost" size="sm" aria-label={`Remove ${name}`} onClick={() => remove(name)}>
										<Trash2 className="h-4 w-4" />
									</Button>
								</div>
							))}
						</div>
					)}
				</>
			)}
		</div>
	);
}