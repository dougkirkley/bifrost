import type {
	GetRelaysParams,
	GetRelaysResponse,
	Relay,
	RelayFilterData,
	RelayHistogramResponse,
	RelayLog,
	RelayLogFilters,
	RelayLogPagination,
	RelayLogSearchResponse,
	RelayLogStats,
	UsagePreview,
	UsagePreviewRequest,
} from "@/lib/types/relays";
import { getRangeForPeriod } from "@/lib/utils/timeRange";
import { baseApi } from "./baseApi";

const MULTI_FILTER_KEYS = [
	"relay_names",
	"methods",
	"statuses",
	"kinds",
	"virtual_key_ids",
	"user_ids",
	"team_ids",
	"customer_ids",
	"business_unit_ids",
	"project_ids",
] as const;

// Shared by the list, stats and histogram queries so all three narrow to the same rows. A relative
// period is expanded to a fresh window per request, so polling always queries a rolling window.
export function buildRelayLogQuery(filters: RelayLogFilters, pagination?: RelayLogPagination): string {
	const params = new URLSearchParams();
	if (pagination) {
		params.set("limit", String(pagination.limit));
		params.set("offset", String(pagination.offset));
		params.set("sort_by", pagination.sort_by);
		params.set("order", pagination.order);
	}
	for (const key of MULTI_FILTER_KEYS) {
		for (const value of filters[key] ?? []) {
			if (value) params.append(key, value);
		}
	}
	if (filters.billable !== undefined) params.set("billable", String(filters.billable));
	if (filters.search) params.set("search", filters.search);
	if (filters.period) {
		const { from, to } = getRangeForPeriod(filters.period);
		params.set("start_time", from.toISOString());
		params.set("end_time", to.toISOString());
	} else if (filters.start_time && filters.end_time) {
		params.set("start_time", filters.start_time);
		params.set("end_time", filters.end_time);
	}
	return params.toString();
}

export const relaysApi = baseApi.injectEndpoints({
	endpoints: (builder) => ({
		// Without params every relay is listed; the selector passes search and a page.
		getRelays: builder.query<GetRelaysResponse, GetRelaysParams | void>({
			query: (params) => ({ url: "/relays", params: params ?? undefined }),
			providesTags: (result) => ["Relays", ...(result?.relays ?? []).map((e) => ({ type: "Relays" as const, id: e.name }))],
		}),
		getRelay: builder.query<Relay, string>({
			query: (name) => ({ url: `/relays/${encodeURIComponent(name)}` }),
			providesTags: (_r, _e, name) => [{ type: "Relays", id: name }],
		}),
		createRelay: builder.mutation<Relay, Relay>({
			query: (body) => ({ url: "/relays", method: "POST", body }),
			invalidatesTags: ["Relays"],
		}),
		updateRelay: builder.mutation<Relay, { name: string; data: Relay }>({
			query: ({ name, data }) => ({ url: `/relays/${encodeURIComponent(name)}`, method: "PUT", body: data }),
			invalidatesTags: (_r, _e, { name }) => [{ type: "Relays", id: name }],
		}),
		deleteRelay: builder.mutation<void, string>({
			query: (name) => ({ url: `/relays/${encodeURIComponent(name)}`, method: "DELETE" }),
			// Deleting an endpoint cascades its virtual-key grants.
			invalidatesTags: ["Relays", "VirtualKeys"],
		}),
		previewRelayUsage: builder.mutation<UsagePreview, UsagePreviewRequest>({
			query: (body) => ({ url: "/relays/usage-preview", method: "POST", body }),
		}),

		getRelayLogs: builder.query<RelayLogSearchResponse, { filters: RelayLogFilters; pagination: RelayLogPagination }>({
			query: ({ filters, pagination }) => ({ url: `/relay-logs?${buildRelayLogQuery(filters, pagination)}` }),
			providesTags: ["RelayLogs"],
		}),
		getRelayLog: builder.query<RelayLog, string>({
			query: (id) => ({ url: `/relay-logs/${encodeURIComponent(id)}` }),
			providesTags: (_r, _e, id) => [{ type: "RelayLogs", id }],
		}),
		getRelayLogStats: builder.query<RelayLogStats, { filters: RelayLogFilters }>({
			query: ({ filters }) => ({ url: `/relay-logs/stats?${buildRelayLogQuery(filters)}` }),
			providesTags: ["RelayLogs"],
		}),
		getRelayLogHistogram: builder.query<RelayHistogramResponse, { filters: RelayLogFilters; bucketSizeSeconds?: number }>({
			query: ({ filters, bucketSizeSeconds }) => {
				const query = buildRelayLogQuery(filters);
				const bucket = bucketSizeSeconds ? `bucket_size_seconds=${bucketSizeSeconds}` : "";
				return { url: `/relay-logs/histogram?${[query, bucket].filter(Boolean).join("&")}` };
			},
			providesTags: ["RelayLogs"],
		}),
		getRelayLogFilterData: builder.query<RelayFilterData, void>({
			query: () => ({ url: "/relay-logs/filterdata" }),
			providesTags: ["RelayLogs"],
		}),
		deleteRelayLogs: builder.mutation<void, string[]>({
			query: (ids) => ({ url: "/relay-logs", method: "DELETE", body: { ids } }),
			invalidatesTags: ["RelayLogs"],
		}),
	}),
});

export const {
	useGetRelaysQuery,
	useGetRelayQuery,
	useCreateRelayMutation,
	useUpdateRelayMutation,
	useDeleteRelayMutation,
	usePreviewRelayUsageMutation,
	useGetRelayLogsQuery,
	useGetRelayLogQuery,
	useGetRelayLogStatsQuery,
	useGetRelayLogHistogramQuery,
	useGetRelayLogFilterDataQuery,
	useDeleteRelayLogsMutation,
} = relaysApi;