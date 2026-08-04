/**
 * Kiro bills credits, not tokens, so omp's cost and token segments read `0` for
 * every kiro turn. This module surfaces the credit pool instead: a `credits
 * <used>/<total>` readout in the status line while a kiro model is active, plus
 * `/kiro-credits` for the full breakdown on demand.
 *
 * Status-line text goes through `ctx.ui.setStatus`, the one extension-facing
 * status surface — omp's built-in segments are a closed union and the `/usage`
 * fetchers are a hardcoded table, neither of which extensions can extend.
 */

import type { ExtensionAPI, ExtensionContext } from "@oh-my-pi/pi-coding-agent";
import { fetchCredits, type KiroCredits, PROVIDER, type ProxyTarget } from "./proxy.ts";

/** `setStatus` key. Hook statuses render sorted by key, so this is also the sort position. */
const STATUS_KEY = "kiro-credits";

/**
 * Floor between upstream calls. Every `/v1/usage` makes the proxy call Kiro's
 * usage-limits API, so a burst of short turns must not hammer it.
 */
const MIN_REFRESH_MS = 15_000;

let cache: { value: KiroCredits; at: number } | undefined;
let inFlight: Promise<KiroCredits> | undefined;

/** Throttled, request-coalescing read. `force` skips the cache but still coalesces. */
async function getCredits(target: ProxyTarget, force: boolean): Promise<KiroCredits> {
	if (!force && cache && Date.now() - cache.at < MIN_REFRESH_MS) return cache.value;
	if (inFlight) return inFlight;

	inFlight = fetchCredits(target.baseUrl, target.apiKey)
		.then((value) => {
			cache = { value, at: Date.now() };
			return value;
		})
		.finally(() => {
			inFlight = undefined;
		});
	return inFlight;
}

/** Thousands separators with a pinned locale: a `de-DE` default renders `10.000`, which reads as a decimal. */
function n(value: number): string {
	return value.toLocaleString("en-US");
}

/**
 * Push the current balance into the status line, or clear it when the active
 * model is not a kiro model. Failures keep the last good value: the status line
 * is glanceable and must not flicker or show an error string — `/kiro-credits`
 * is where a failure is reported out loud.
 */
async function refreshStatus(ctx: ExtensionContext, target: ProxyTarget): Promise<void> {
	if (!ctx.hasUI) return;
	if (ctx.model?.provider !== PROVIDER) {
		ctx.ui.setStatus(STATUS_KEY, undefined);
		return;
	}
	try {
		const { used, total } = await getCredits(target, false);
		ctx.ui.setStatus(STATUS_KEY, ctx.ui.theme.fg("muted", `credits ${n(used)}/${n(total)}`));
	} catch {
		// Keep whatever is already rendered.
	}
}

/** Wire the status-line refresh and the `/kiro-credits` command onto an already-resolved proxy target. */
export function registerCredits(pi: ExtensionAPI, target: ProxyTarget): void {
	// Fire-and-forget: a slow proxy must never stall session start or a turn.
	// No polling interval — credits only move when a turn spends them.
	pi.on("session_start", (_event, ctx) => {
		void refreshStatus(ctx, target);
	});
	pi.on("turn_end", (_event, ctx) => {
		void refreshStatus(ctx, target);
	});

	pi.registerCommand("kiro-credits", {
		description: "Show Kiro credit balance",
		handler: async (_args, ctx) => {
			try {
				// Forced: an explicit command must never show a cached number.
				const { used, total, remaining } = await getCredits(target, true);
				const free = total > 0 ? ` (${((remaining / total) * 100).toFixed(1)}% free)` : "";
				ctx.ui.notify(`Kiro credits: ${n(used)} / ${n(total)} used · ${n(remaining)} left${free}`, "info");
				// Cache is fresh from the forced fetch, so this does no extra HTTP.
				await refreshStatus(ctx, target);
			} catch (error) {
				ctx.ui.notify(
					`Kiro credits unavailable: ${error instanceof Error ? error.message : String(error)}`,
					"error",
				);
			}
		},
	});
}
