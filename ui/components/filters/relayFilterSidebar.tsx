import { FilterSidebarTrigger } from "@/components/filters/filterSidebarTrigger";
import { CheckboxFilterItem, FilterSection, SearchableCheckboxList, useAutoFocusOnOpen } from "@/components/filters/primitives";
import { Button } from "@/components/ui/button";
import { ScrollArea } from "@/components/ui/scrollArea";
import { useIsMobile } from "@/hooks/use-mobile";
import { useGetRelayLogFilterDataQuery } from "@/lib/store";
import type { RelayLogFilters } from "@/lib/types/relays";
import { PanelLeftClose, RotateCcw } from "lucide-react";
import { useCallback, useEffect, useMemo, useState } from "react";

const COLLAPSE_STORAGE_KEY = "relay-filter-sidebar-collapsed";

interface RelayFilterSidebarProps {
	filters: RelayLogFilters;
	onFiltersChange: (filters: RelayLogFilters) => void;
}

type MultiKey = "relay_names" | "methods" | "statuses" | "kinds" | "virtual_key_ids";

/** The Relay Logs filter sidebar, built on the same primitives as the LLM and MCP log sidebars. */
export function RelayFilterSidebar({ filters, onFiltersChange }: RelayFilterSidebarProps) {
	const isMobile = useIsMobile();
	const [collapsed, setCollapsed] = useState(false);
	const [opened, setOpened] = useState(false);
	const endpointSearchRef = useAutoFocusOnOpen(opened);
	const { data } = useGetRelayLogFilterDataQuery();

	useEffect(() => {
		if (typeof window === "undefined") return;
		if (isMobile) {
			setCollapsed(true);
			return;
		}
		setCollapsed(window.localStorage.getItem(COLLAPSE_STORAGE_KEY) === "true");
	}, [isMobile]);

	const toggleCollapsed = useCallback(() => {
		setCollapsed((prev) => {
			const next = !prev;
			if (typeof window !== "undefined") window.localStorage.setItem(COLLAPSE_STORAGE_KEY, String(next));
			return next;
		});
	}, []);

	const activeFilterCount = useMemo(() => {
		const excluded = ["start_time", "end_time", "search", "period"];
		return Object.entries(filters).reduce((c, [key, value]) => {
			if (excluded.includes(key)) return c;
			if (Array.isArray(value)) return c + value.length;
			return c + (value !== undefined && value !== "" ? 1 : 0);
		}, 0);
	}, [filters]);

	const toggle = (key: MultiKey, value: string) => {
		const current = filters[key] ?? [];
		const next = current.includes(value) ? current.filter((v) => v !== value) : [...current, value];
		onFiltersChange({ ...filters, [key]: next });
	};

	const handleReset = useCallback(() => {
		onFiltersChange({ start_time: filters.start_time, end_time: filters.end_time, period: filters.period });
	}, [filters.start_time, filters.end_time, filters.period, onFiltersChange]);

	if (collapsed) {
		return <FilterSidebarTrigger activeFilterCount={activeFilterCount} onClick={toggleCollapsed} testId="relay-filter-sidebar-trigger" />;
	}

	const endpointItems = [...new Set([...(data?.relay_names ?? []), ...(filters.relay_names ?? [])])]
		.sort()
		.map((n) => ({ key: n, label: n }));
	const vkItems = (data?.virtual_keys ?? []).map((vk) => ({ key: vk.id, label: vk.name || vk.id }));

	return (
		<div
			className="bg-card fixed inset-y-2 left-2 z-40 flex h-auto w-[calc(100vw-1rem)] max-w-72 shrink-0 flex-col rounded-md border shadow-xl md:static md:h-full md:w-64 md:max-w-none md:rounded-md md:shadow-none"
			data-testid="relay-filter-sidebar"
		>
			<div className="flex h-11 items-center justify-between border-b pr-2 pl-5">
				<span className="text-sm font-semibold">Filters</span>
				<div className="flex items-center gap-1">
					{activeFilterCount > 0 && (
						<Button variant="outline" size="sm" className="text-muted-foreground h-7 px-2 text-xs" onClick={handleReset}>
							<RotateCcw className="size-3" />
							Reset
						</Button>
					)}
					<Button variant="ghost" size="icon" className="size-7" onClick={toggleCollapsed} title="Hide filters" aria-label="Hide filters">
						<PanelLeftClose className="size-4" />
					</Button>
				</div>
			</div>
			<ScrollArea className="flex flex-1 overflow-y-auto p-2 pb-0" viewportClassName="no-table">
				<div className="flex grow flex-col gap-1">
					<FilterSection title="Status" defaultOpen>
						{["success", "error", "cancelled"].map((status) => (
							<CheckboxFilterItem
								key={status}
								labelClassName="capitalize"
								label={status}
								checked={(filters.statuses ?? []).includes(status)}
								onCheckedChange={() => toggle("statuses", status)}
							/>
						))}
					</FilterSection>
					<FilterSection title="Relay" defaultOpen onOpenChange={setOpened}>
						<SearchableCheckboxList
							inputRef={endpointSearchRef}
							placeholder="Search relays"
							items={endpointItems}
							isSelected={(n) => (filters.relay_names ?? []).includes(n)}
							onToggle={(n) => toggle("relay_names", n)}
							testIdPrefix="relay-filter"
							normalizeTestIdKey
						/>
					</FilterSection>
					<FilterSection title="Method" defaultOpen={(filters.methods ?? []).length > 0}>
						{(data?.methods ?? []).map((m) => (
							<CheckboxFilterItem
								key={m}
								label={m}
								checked={(filters.methods ?? []).includes(m)}
								onCheckedChange={() => toggle("methods", m)}
							/>
						))}
					</FilterSection>
					<FilterSection title="Billing" defaultOpen={filters.billable !== undefined}>
						<CheckboxFilterItem
							label="Billable"
							checked={filters.billable === true}
							onCheckedChange={() => onFiltersChange({ ...filters, billable: filters.billable === true ? undefined : true })}
						/>
						<CheckboxFilterItem
							label="Not billable"
							checked={filters.billable === false}
							onCheckedChange={() => onFiltersChange({ ...filters, billable: filters.billable === false ? undefined : false })}
						/>
					</FilterSection>
					<FilterSection title="Kind" defaultOpen={(filters.kinds ?? []).length > 0}>
						<CheckboxFilterItem
							label="Calls"
							checked={(filters.kinds ?? []).includes("call")}
							onCheckedChange={() => toggle("kinds", "call")}
						/>
						<CheckboxFilterItem
							label="Job settlements"
							checked={(filters.kinds ?? []).includes("job")}
							onCheckedChange={() => toggle("kinds", "job")}
						/>
					</FilterSection>
					<FilterSection title="Virtual key" defaultOpen={(filters.virtual_key_ids ?? []).length > 0}>
						<SearchableCheckboxList
							placeholder="Search virtual keys"
							items={vkItems}
							isSelected={(id) => (filters.virtual_key_ids ?? []).includes(id)}
							onToggle={(id) => toggle("virtual_key_ids", id)}
							testIdPrefix="relay-vk-filter"
							normalizeTestIdKey
						/>
					</FilterSection>
				</div>
			</ScrollArea>
		</div>
	);
}