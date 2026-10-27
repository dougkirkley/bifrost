import { AttributionCell } from "@/components/logAttributionCell";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdownMenu";
import { Status, StatusBarColors, Statuses } from "@/lib/constants/logs";
import type { RelayLog } from "@/lib/types/relays";
import { ColumnDef, Row } from "@tanstack/react-table";
import { format, formatDistanceToNow, isValid } from "date-fns";
import { ArrowUpDown, MoreHorizontal, Trash2 } from "lucide-react";

const statusOf = (status: string): Status => (Statuses.includes(status as Status) ? (status as Status) : "processing");

export const RELAY_COLUMN_LABELS: Record<string, string> = {
	timestamp: "Time",
	relay_name: "Relay",
	request: "Request",
	status_code: "Status",
	cost: "Cost",
	units: "Units",
	latency: "Latency",
	virtual_key: "Virtual Key",
	user: "User",
	team: "Team",
	customer: "Customer",
	business_unit: "Business Unit",
	project: "Project",
};

const sortableHeader =
	(label: string) =>
	({ column }: { column: { toggleSorting: (desc?: boolean) => void; getIsSorted: () => false | "asc" | "desc" } }) => (
		<Button variant="ghost" onClick={() => column.toggleSorting(column.getIsSorted() === "asc")}>
			{label}
			<ArrowUpDown className="ml-2 h-4 w-4" />
		</Button>
	);

/** Columns for the Relay Logs table, laid out like the LLM and MCP log tables. */
export const createRelayColumns = (handleDelete: (log: RelayLog) => Promise<void>, hasDeleteAccess: boolean): ColumnDef<RelayLog>[] => [
	{
		accessorKey: "status",
		header: "",
		size: 8,
		maxSize: 8,
		cell: ({ row }) => (
			<div title={row.original.status} className={`h-full min-h-[24px] w-1 rounded-sm ${StatusBarColors[statusOf(row.original.status)]}`} />
		),
	},
	{
		accessorKey: "timestamp",
		header: sortableHeader("Time"),
		size: 130,
		cell: ({ row }) => {
			const date = row.original.timestamp ? new Date(row.original.timestamp) : null;
			if (!date || !isValid(date)) return <div className="truncate text-xs">N/A</div>;
			return (
				<div className="flex flex-col leading-tight">
					<span className="font-mono text-xs tabular-nums">{format(date, "MMM dd  HH:mm:ss")}</span>
					<span className="text-muted-foreground text-[10.5px] tabular-nums">{formatDistanceToNow(date, { addSuffix: true })}</span>
				</div>
			);
		},
	},
	{
		accessorKey: "relay_name",
		header: "Relay",
		size: 150,
		cell: ({ row }) => (
			<Badge variant="secondary" className="font-mono">
				{row.original.relay_name}
			</Badge>
		),
	},
	{
		id: "request",
		header: "Request",
		size: 300,
		cell: ({ row }) => {
			const log = row.original;
			if (log.kind === "job") {
				return (
					<div className="flex min-w-0 flex-col leading-tight">
						<span className="truncate font-mono text-xs">job {log.job_id}</span>
						<span className="text-muted-foreground truncate text-[10.5px]">Settled: {log.job_status}</span>
					</div>
				);
			}
			return (
				<div className="flex min-w-0 flex-col leading-tight">
					<span className="truncate font-mono text-xs">
						{log.method} {log.path}
					</span>
					<span className="text-muted-foreground truncate text-[10.5px]">
						{log.route_path ? `Route ${log.route_path}` : "No route"}
						{log.streamed ? " · streamed" : ""}
						{log.billable ? "" : " · not billable"}
					</span>
				</div>
			);
		},
	},
	{
		accessorKey: "status_code",
		header: "Status",
		size: 90,
		cell: ({ row }) => <span className="font-mono text-xs">{row.original.status_code || row.original.status}</span>,
	},
	{
		accessorKey: "latency",
		header: sortableHeader("Latency"),
		size: 120,
		cell: ({ row }) => (
			<div className="pl-4 font-mono text-xs tabular-nums">
				{row.original.latency != null ? `${Math.round(row.original.latency).toLocaleString()}ms` : "-"}
			</div>
		),
	},
	{
		accessorKey: "cost",
		header: sortableHeader("Cost"),
		size: 120,
		cell: ({ row }) => {
			const cost = row.original.cost;
			return <div className="pl-4 font-mono text-sm">{typeof cost === "number" && Number.isFinite(cost) ? cost.toFixed(4) : "N/A"}</div>;
		},
	},
	{
		id: "units",
		header: "Units",
		size: 100,
		cell: ({ row }) => <div className="font-mono text-xs">{row.original.billed_units ?? "-"}</div>,
	},
	{
		id: "virtual_key",
		header: "Virtual Key",
		size: 170,
		cell: ({ row }) => (
			<div className="max-w-[180px] truncate font-mono text-xs">{row.original.virtual_key_name || row.original.virtual_key_id || "-"}</div>
		),
	},
	{ id: "user", header: "User", size: 150, cell: ({ row }) => <AttributionCell name={row.original.user_name} id={row.original.user_id} /> },
	{ id: "team", header: "Team", size: 150, cell: ({ row }) => <AttributionCell name={row.original.team_name} id={row.original.team_id} /> },
	{
		id: "customer",
		header: "Customer",
		size: 150,
		cell: ({ row }) => <AttributionCell name={row.original.customer_name} id={row.original.customer_id} />,
	},
	{
		id: "business_unit",
		header: "Business Unit",
		size: 150,
		cell: ({ row }) => <AttributionCell name={row.original.business_unit_name} id={row.original.business_unit_id} />,
	},
	{
		id: "project",
		header: "Project",
		size: 150,
		cell: ({ row }) => <AttributionCell name={row.original.project_name} id={row.original.project_id} />,
	},
	...(hasDeleteAccess
		? [
				{
					id: "actions",
					header: "",
					size: 56,
					cell: ({ row }: { row: Row<RelayLog> }) => (
						<div className="flex justify-center">
							<DropdownMenu>
								<DropdownMenuTrigger asChild onClick={(event) => event.stopPropagation()}>
									<Button variant="ghost" size="icon" data-testid="log-actions-btn" aria-label="Log actions" className="h-7 w-7">
										<MoreHorizontal className="h-4 w-4" />
									</Button>
								</DropdownMenuTrigger>
								<DropdownMenuContent align="end">
									<DropdownMenuItem
										variant="destructive"
										className="cursor-pointer"
										data-testid="log-delete-btn"
										onClick={(event) => {
											event.stopPropagation();
											handleDelete(row.original).catch(() => undefined); // the page shows the error
										}}
									>
										<Trash2 className="h-4 w-4" />
										Delete
									</DropdownMenuItem>
								</DropdownMenuContent>
							</DropdownMenu>
						</div>
					),
				},
			]
		: []),
];