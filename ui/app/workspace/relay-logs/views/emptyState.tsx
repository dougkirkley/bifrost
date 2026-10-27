import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { CodeEditor } from "@/components/ui/codeEditor";
import { useCopyToClipboard } from "@/hooks/useCopyToClipboard";
import { getExampleBaseUrl } from "@/lib/utils/port";
import { Link } from "@tanstack/react-router";
import { AlertTriangle, ArrowRight, Copy } from "lucide-react";
import { useMemo } from "react";

const EDITOR_OPTIONS = {
	scrollBeyondLastLine: false,
	minimap: { enabled: false },
	lineNumbers: "off",
	folding: false,
	lineDecorationsWidth: 0,
	lineNumbersMinChars: 0,
	glyphMargin: false,
} as const;

function CodeBlock({ code }: { code: string }) {
	const { copy } = useCopyToClipboard();

	return (
		<div className="relative">
			<Button variant="ghost" size="icon" className="absolute top-4 right-4 z-10" onClick={() => copy(code)} aria-label="Copy request">
				<Copy className="size-4" />
			</Button>
			<CodeEditor className="w-full" code={code} lang="shell" readonly height={220} fontSize={14} options={EDITOR_OPTIONS} />
		</div>
	);
}

export function RelayEmptyState({ error }: { error?: string | null }) {
	const examples = useMemo(() => {
		const baseUrl = getExampleBaseUrl();
		return {
			request: `curl -X POST "${baseUrl}/relay/<relay-name>/<path>" \\
  -H "Content-Type: application/json" \\
  -H "x-bf-vk: <virtual-key>" \\
  -d '{
    "query": "Hello from Bifrost"
  }'`,
		};
	}, []);

	const isUnexpectedError = error && error.includes("An unexpected error occurred");

	return (
		<div className="dark:bg-card flex w-full flex-col items-center justify-center space-y-8 bg-white" data-testid="relay-logs-empty">
			{error && (
				<Alert>
					<AlertTriangle className="h-4 w-4" />
					<AlertDescription>
						{isUnexpectedError ? "Looks like you haven't configured the log store in your config file." : error}
					</AlertDescription>
				</Alert>
			)}

			<div className="w-full space-y-6 p-4">
				<div className="flex items-center gap-4">
					<div>
						<h3 className="text-lg font-semibold">Send your first relay request</h3>
						<p className="text-muted-foreground text-sm">
							Relay a call to an upstream API through Bifrost to start collecting Relay Logs with their cost.
						</p>
					</div>
					<Button asChild variant="outline" className="ml-auto">
						<Link to="/workspace/relays">
							Open Relays
							<ArrowRight className="size-4" />
						</Link>
					</Button>
				</div>

				<div className="space-y-3">
					<p className="text-muted-foreground text-sm">
						The request is relayed as-is to the relay&apos;s base URL, with the relay&apos;s credential added.
					</p>
					<CodeBlock code={examples.request} />
				</div>

				<div className="bg-muted/50 rounded-lg border p-4">
					<h4 className="mb-2 text-sm font-semibold">Before you send a request</h4>
					<ol className="text-muted-foreground space-y-2 text-sm">
						<li className="flex items-start gap-2">
							<span className="text-primary">1.</span>
							<span>Add a relay in Relays with its base URL and credential.</span>
						</li>
						<li className="flex items-start gap-2">
							<span className="text-primary">2.</span>
							<span>
								Replace <code className="bg-muted rounded px-1">&lt;relay-name&gt;</code> with its name and{" "}
								<code className="bg-muted rounded px-1">&lt;path&gt;</code> with one of its routes, or turn on Relay Any Path.
							</span>
						</li>
						<li className="flex items-start gap-2">
							<span className="text-primary">3.</span>
							<span>
								Use a virtual key that grants the endpoint (or all relays), or remove the{" "}
								<code className="bg-muted rounded px-1">x-bf-vk</code> header when authentication is disabled.
							</span>
						</li>
					</ol>
				</div>
			</div>
		</div>
	);
}