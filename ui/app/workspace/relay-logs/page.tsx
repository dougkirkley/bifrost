import { LogsVolumeChart } from "@/app/workspace/logs/views/logsVolumeChart";
import { RelayFilterSidebar } from "@/components/filters/relayFilterSidebar";
import FullPageLoader from "@/components/fullPageLoader";
import { useColumnConfig } from "@/components/table";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Card, CardContent } from "@/components/ui/card";
import { parseAsSafeString } from "@/lib/queryParamsParser";
import {
	getErrorMessage,
	useDeleteRelayLogsMutation,
	useGetRelayLogHistogramQuery,
	useGetRelayLogsQuery,
	useGetRelayLogStatsQuery,
} from "@/lib/store";
import type { RelayLog, RelayLogFilters, RelayLogPagination } from "@/lib/types/relays";
import { dateUtils } from "@/lib/types/logs";
import { COMPACT_NUMBER_FORMAT } from "@/lib/utils/numbers";
import { getLiveToggleState } from "@/lib/utils/timeRange";
import { RbacOperation, RbacResource, useRbac } from "@enterprise/lib";
import NumberFlow from "@number-flow/react";
import { useLocation } from "@tanstack/react-router";
import { AlertCircle } from "lucide-react";
import { parseAsArrayOf, parseAsBoolean, parseAsInteger, parseAsString, useQueryStates } from "nuqs";
import { useCallback, useMemo, useState } from "react";
import { RelayEmptyState } from "./views/emptyState";
import { createRelayColumns, RELAY_COLUMN_LABELS } from "./views/columns";
import { RelayHeaderView } from "./views/relayHeaderView";
import { RelayLogDetailSheet } from "./views/relayLogDetailsSheet";
import { RelayLogsDataTable } from "./views/relayLogsTable";

const arr = parseAsArrayOf(parseAsString).withDefault([]);

/** Relay Logs: the same layout, header, filters, chart and table as the LLM and MCP log pages. */
export default function RelayLogsPage() {
	const [error, setError] = useState<string | null>(null);
	const hasDeleteAccess = useRbac(RbacResource.Logs, RbacOperation.Delete);
	const [deleteLogs] = useDeleteRelayLogsMutation();
	const defaultTimeRange = useMemo(() => dateUtils.getDefaultTimeRange(), []);
	const { search } = useLocation();
	const hasExplicitTimeRange = (search as Record<string, unknown>)?.start_time && (search as Record<string, unknown>)?.end_time;

	const [urlState, setUrlState] = useQueryStates(
		{
			relay_names: arr,
			methods: arr,
			statuses: arr,
			kinds: arr,
			virtual_key_ids: arr,
			user_ids: arr,
			team_ids: arr,
			customer_ids: arr,
			business_unit_ids: arr,
			project_ids: arr,
			billable: parseAsString.withDefault(""),
			search: parseAsSafeString.withDefault(""),
			start_time: parseAsInteger.withDefault(defaultTimeRange.startTime),
			end_time: parseAsInteger.withDefault(defaultTimeRange.endTime),
			limit: parseAsInteger.withDefault(50),
			offset: parseAsInteger.withDefault(0),
			sort_by: parseAsString.withDefault("timestamp"),
			order: parseAsString.withDefault("desc"),
			polling: parseAsBoolean.withDefault(true).withOptions({ clearOnDefault: false }),
			period: parseAsString.withDefault(hasExplicitTimeRange ? "" : "1h").withOptions({ clearOnDefault: false }),
			selected_log: parseAsString.withDefault(""),
		},
		{ history: "push", shallow: false },
	);
	const polling = urlState.polling;
	const [isChartOpen, setIsChartOpen] = useState(true);

	const filters: RelayLogFilters = useMemo(
		() => ({
			relay_names: urlState.relay_names,
			methods: urlState.methods,
			statuses: urlState.statuses,
			kinds: urlState.kinds,
			virtual_key_ids: urlState.virtual_key_ids,
			user_ids: urlState.user_ids,
			team_ids: urlState.team_ids,
			customer_ids: urlState.customer_ids,
			business_unit_ids: urlState.business_unit_ids,
			project_ids: urlState.project_ids,
			billable: urlState.billable === "" ? undefined : urlState.billable === "true",
			search: urlState.search || undefined,
			...(urlState.period
				? { period: urlState.period }
				: { start_time: dateUtils.toISOString(urlState.start_time), end_time: dateUtils.toISOString(urlState.end_time) }),
		}),
		[urlState],
	);
	const pagination: RelayLogPagination = useMemo(
		() => ({
			limit: urlState.limit,
			offset: urlState.offset,
			sort_by: urlState.sort_by as RelayLogPagination["sort_by"],
			order: urlState.order as "asc" | "desc",
		}),
		[urlState.limit, urlState.offset, urlState.sort_by, urlState.order],
	);

	const pollingOptions = { pollingInterval: polling ? 10000 : 0, skipPollingIfUnfocused: true };
	const {
		data: logsData,
		isLoading: logsIsLoading,
		isFetching: logsIsFetching,
		error: logsError,
		refetch: refetchLogs,
	} = useGetRelayLogsQuery({ filters, pagination }, pollingOptions);
	const { data: statsData, isFetching: statsIsFetching, refetch: refetchStats } = useGetRelayLogStatsQuery({ filters }, pollingOptions);
	const {
		data: histogram,
		isLoading: histogramIsLoading,
		refetch: refetchHistogram,
	} = useGetRelayLogHistogramQuery({ filters }, pollingOptions);

	const refreshAllData = useCallback(() => {
		refetchLogs();
		refetchStats();
		refetchHistogram();
	}, [refetchLogs, refetchStats, refetchHistogram]);

	const handleTimeRangeChange = useCallback(
		(startTime: number, endTime: number) =>
			setUrlState({ period: "", start_time: startTime, end_time: endTime, offset: 0, polling: false }),
		[setUrlState],
	);
	const handleResetZoom = useCallback(() => {
		const now = Math.floor(Date.now() / 1000);
		setUrlState({ period: "1h", start_time: now - 3600, end_time: now, offset: 0, polling: true });
	}, [setUrlState]);
	const isZoomed = useMemo(
		() => !urlState.period && urlState.end_time - urlState.start_time < 3600 * 0.9,
		[urlState.start_time, urlState.end_time, urlState.period],
	);

	const logs = useMemo(() => logsData?.logs ?? [], [logsData]);
	const totalItems = logsData?.pagination?.total_count ?? 0;
	const selectedLogId = urlState.selected_log || null;
	const selectedLogIndex = useMemo(() => (selectedLogId ? logs.findIndex((l) => l.id === selectedLogId) : -1), [selectedLogId, logs]);
	const selectedLog = selectedLogIndex >= 0 ? logs[selectedLogIndex] : null;

	const setFilters = useCallback(
		(next: RelayLogFilters) => {
			const timeChanged = next.start_time !== undefined || next.end_time !== undefined;
			setUrlState({
				...(timeChanged && !next.period ? { period: "" } : {}),
				relay_names: next.relay_names ?? [],
				methods: next.methods ?? [],
				statuses: next.statuses ?? [],
				kinds: next.kinds ?? [],
				virtual_key_ids: next.virtual_key_ids ?? [],
				user_ids: next.user_ids ?? [],
				team_ids: next.team_ids ?? [],
				customer_ids: next.customer_ids ?? [],
				business_unit_ids: next.business_unit_ids ?? [],
				project_ids: next.project_ids ?? [],
				billable: next.billable === undefined ? "" : String(next.billable),
				search: next.search ?? "",
				offset: 0,
			});
		},
		[setUrlState],
	);
	const setPagination = useCallback(
		(p: RelayLogPagination) => setUrlState({ limit: p.limit, offset: p.offset, sort_by: p.sort_by, order: p.order }),
		[setUrlState],
	);
	const handleDelete = useCallback(
		async (log: RelayLog) => {
			setError(null);
			try {
				await deleteLogs([log.id]).unwrap();
				if (urlState.selected_log === log.id) setUrlState({ selected_log: "" });
				refreshAllData();
			} catch (err) {
				setError(getErrorMessage(err));
				// Rethrown so the details sheet stays open on a failed delete.
				throw err;
			}
		},
		[deleteLogs, urlState.selected_log, setUrlState, refreshAllData],
	);
	const handlePeriodChange = useCallback(
		(p?: string, from?: Date, to?: Date) => {
			if (p) setUrlState({ period: p, offset: 0, polling: true });
			else if (from && to)
				setUrlState({
					start_time: Math.floor(from.getTime() / 1000),
					end_time: Math.floor(to.getTime() / 1000),
					offset: 0,
					polling: false,
					period: "",
				});
		},
		[setUrlState],
	);
	const handlePollToggle = useCallback(
		(enabled: boolean) => {
			const next = getLiveToggleState(enabled, urlState.period);
			setUrlState(next);
			if (enabled && !next.period) refreshAllData();
		},
		[setUrlState, refreshAllData, urlState.period],
	);
	const handleLogNavigate = useCallback(
		(direction: "prev" | "next") => {
			const target = direction === "prev" ? selectedLogIndex - 1 : selectedLogIndex + 1;
			if (target >= 0 && target < logs.length) setUrlState({ selected_log: logs[target].id }, { history: "replace" });
		},
		[selectedLogIndex, logs, setUrlState],
	);

	const statCards = [
		{
			title: "Total Requests",
			value: <NumberFlow value={statsData?.total_requests ?? 0} format={COMPACT_NUMBER_FORMAT} />,
			testId: "relay-logs-stat-requests",
		},
		{
			title: "Success Rate",
			value: <NumberFlow value={statsData?.success_rate ?? 0} format={{ minimumFractionDigits: 2, maximumFractionDigits: 2 }} suffix="%" />,
			testId: "relay-logs-stat-success",
		},
		{
			title: "Avg Latency",
			value: (
				<NumberFlow value={statsData?.average_latency ?? 0} format={{ minimumFractionDigits: 2, maximumFractionDigits: 2 }} suffix="ms" />
			),
			testId: "relay-logs-stat-latency",
		},
		{
			title: "Total Cost",
			value: <NumberFlow value={statsData?.total_cost ?? 0} format={{ ...COMPACT_NUMBER_FORMAT, style: "currency", currency: "USD" }} />,
			testId: "relay-logs-stat-cost",
		},
	];

	const columns = useMemo(() => createRelayColumns(handleDelete, hasDeleteAccess), [handleDelete, hasDeleteAccess]);
	const columnIds = useMemo(
		() => columns.map((c) => ("id" in c && c.id ? c.id : "accessorKey" in c ? String(c.accessorKey) : "")).filter(Boolean),
		[columns],
	);
	const {
		entries: columnEntries,
		columnOrder,
		columnVisibility,
		columnPinning,
		toggleVisibility,
		togglePin,
		reorder,
		reset,
	} = useColumnConfig({
		columnIds,
		paramName: "relay_cols",
		storageKey: "bifrost.relay_logs.cols",
		defaultHidden: ["user", "team", "customer", "business_unit", "project"],
		fixedColumns: hasDeleteAccess ? { right: ["actions"] } : undefined,
	});

	const displayError = error ?? (logsError ? getErrorMessage(logsError as Parameters<typeof getErrorMessage>[0]) : null);

	if (logsIsLoading) return <FullPageLoader />;
	if (logsData && !logsData.has_logs) return <RelayEmptyState error={displayError} />;

	return (
		<div className="dark:bg-card bg-white" data-testid="relay-logs-page">
			<div className="no-padding-parent no-border-parent bg-background flex h-[calc(var(--app-content-viewport)_-_var(--app-bottom-padding))] w-full gap-3">
				<RelayFilterSidebar filters={filters} onFiltersChange={setFilters} />
				<div className="bg-card flex min-w-0 flex-1 flex-col gap-2 overflow-hidden rounded-md border">
					<div className="p-4 pb-0">
						<RelayHeaderView
							filters={filters}
							onFiltersChange={setFilters}
							period={urlState.period}
							onPeriodChange={handlePeriodChange}
							polling={polling}
							onPollToggle={handlePollToggle}
							onRefresh={refreshAllData}
							loading={logsIsFetching}
							columnEntries={columnEntries}
							columnLabels={RELAY_COLUMN_LABELS}
							onToggleColumnVisibility={toggleVisibility}
							onResetColumns={reset}
						/>
					</div>
					<div className="px-4">
						<div className="grid shrink-0 grid-cols-1 gap-4 md:grid-cols-4">
							{statCards.map((card) => (
								<Card key={card.title} className="py-4 shadow-none" data-testid={card.testId}>
									<CardContent
										className={`flex items-center justify-between px-4 transition-opacity duration-200 ${statsIsFetching ? "opacity-50" : "opacity-100"}`}
									>
										<div className="w-full min-w-0">
											<div className="text-muted-foreground text-xs">{card.title}</div>
											<div className="truncate font-mono text-xl font-medium sm:text-2xl">{card.value}</div>
										</div>
									</CardContent>
								</Card>
							))}
						</div>
						<div className="mt-2">
							<LogsVolumeChart
								data={histogram ?? null}
								loading={histogramIsLoading}
								onTimeRangeChange={handleTimeRangeChange}
								onResetZoom={handleResetZoom}
								isZoomed={isZoomed}
								startTime={urlState.start_time}
								endTime={urlState.end_time}
								period={urlState.period}
								isOpen={isChartOpen}
								onOpenChange={setIsChartOpen}
							/>
						</div>
						{displayError && (
							<Alert variant="destructive" className="shrink-0">
								<AlertCircle className="h-4 w-4" />
								<AlertDescription>{displayError}</AlertDescription>
							</Alert>
						)}
					</div>
					<RelayLogsDataTable
						columns={columns}
						data={logs}
						totalItems={totalItems}
						loading={logsIsFetching}
						pagination={pagination}
						onPaginationChange={setPagination}
						onRowClick={(row, columnId) => {
							if (columnId === "actions") return;
							setUrlState({ selected_log: row.id }, { history: "replace" });
						}}
						onRefresh={refreshAllData}
						polling={polling}
						columnEntries={columnEntries}
						columnOrder={columnOrder}
						columnVisibility={columnVisibility}
						columnPinning={columnPinning}
						onToggleColumnVisibility={toggleVisibility}
						onTogglePin={togglePin}
						onReorderColumns={reorder}
					/>
				</div>
				<RelayLogDetailSheet
					log={selectedLog}
					logId={selectedLogId}
					open={selectedLogId !== null}
					onOpenChange={(open) => !open && setUrlState({ selected_log: "" }, { history: "replace" })}
					handleDelete={hasDeleteAccess ? handleDelete : undefined}
					onNavigate={handleLogNavigate}
					hasPrev={selectedLogIndex > 0}
					hasNext={selectedLogIndex !== -1 && selectedLogIndex < logs.length - 1}
				/>
			</div>
		</div>
	);
}