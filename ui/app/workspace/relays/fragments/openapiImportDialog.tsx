import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Textarea } from "@/components/ui/textarea";
import { importOpenAPI, parseOpenAPIText, type OpenAPIImport } from "@/lib/utils/openapiImport";
import { useState } from "react";

interface OpenAPIImportDialogProps {
	open: boolean;
	onClose: () => void;
	onImport: (result: OpenAPIImport) => void;
}

/**
 * Turns an OpenAPI or Swagger document (pasted or uploaded, JSON or YAML) into routes, an auth
 * suggestion and a base URL hint. Nothing is saved until the endpoint is.
 */
export function OpenAPIImportDialog({ open, onClose, onImport }: OpenAPIImportDialogProps) {
	const [text, setText] = useState("");
	const [error, setError] = useState<string>();
	const [busy, setBusy] = useState(false);

	const run = async () => {
		setBusy(true);
		setError(undefined);
		try {
			const doc = await parseOpenAPIText(text);
			onImport(importOpenAPI(doc));
			setText("");
			onClose();
		} catch (e) {
			setError(e instanceof Error ? e.message : "The document could not be read");
		} finally {
			setBusy(false);
		}
	};

	return (
		<Dialog open={open} onOpenChange={(o) => !o && onClose()}>
			<DialogContent className="sm:max-w-2xl">
				<DialogHeader>
					<DialogTitle>Import routes from OpenAPI</DialogTitle>
					<DialogDescription>
						Every operation becomes a route. Imported routes replace the current list; review prices before saving.
					</DialogDescription>
				</DialogHeader>
				<input
					type="file"
					accept=".json,.yaml,.yml"
					className="text-sm"
					data-testid="relay-openapi-file"
					onChange={async (e) => {
						const file = e.target.files?.[0];
						if (file) setText(await file.text());
					}}
				/>
				<Textarea
					rows={12}
					value={text}
					onChange={(e) => setText(e.target.value)}
					placeholder="Paste an OpenAPI 3 or Swagger 2 document"
					className="font-mono text-xs"
					data-testid="relay-openapi-text"
				/>
				{error && <p className="text-destructive text-xs">{error}</p>}
				<DialogFooter>
					<Button variant="outline" onClick={onClose}>
						Cancel
					</Button>
					<Button onClick={run} disabled={!text.trim() || busy} isLoading={busy} data-testid="relay-openapi-import">
						Import
					</Button>
				</DialogFooter>
			</DialogContent>
		</Dialog>
	);
}