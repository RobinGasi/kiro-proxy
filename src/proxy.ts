/**
 * Resolution and model metadata for the local `kiro-proxy` OpenAI-compatible
 * endpoint. The proxy fronts Kiro's own backend, so no AWS Bedrock adapter and
 * no vendor SDK are involved — it is a plain `/v1/chat/completions` server that
 * happens to bill in Kiro credits.
 */

import { readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import type { ProviderModelConfig } from "@oh-my-pi/pi-coding-agent";

/** Env var the provider registration points at; see {@link resolveProxy}. */
export const API_KEY_ENV = "KIRO_PROXY_API_KEY";

/** Provider name registered with omp; also the `provider` field on kiro models. */
export const PROVIDER = "kiro";

/**
 * Fallback catalog, used only when the proxy is unreachable *and* omp has no
 * cached model list yet. Keeping it small and current beats an empty provider.
 *
 * Ids only: token limits are left absent so {@link buildModel} resolves them
 * through {@link fallbackLimits} rather than duplicating them here.
 */
const SEED_MODELS: KiroModel[] = [
	"auto-kiro",
	"claude-opus-5",
	"claude-sonnet-5",
	"claude-opus-4.8",
	"claude-sonnet-4.6",
	"claude-haiku-4.5",
	"gpt-5.6-sol",
].map((id) => ({ id }));

/** Read a value from the process env, falling back to `<agentDir>/.env` (`~/.omp/agent/.env`). */
function readAgentEnvValue(name: string, agentDir: string): string | undefined {
	if (process.env[name]) return process.env[name];

	let envText = "";
	try {
		envText = readFileSync(join(agentDir, ".env"), "utf8");
	} catch {
		return undefined;
	}

	for (const line of envText.split(/\r?\n/)) {
		const trimmed = line.trim();
		if (!trimmed || trimmed.startsWith("#")) continue;

		const match = trimmed.match(/^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$/);
		if (!match || match[1] !== name) continue;

		const value = match[2].trim();
		if ((value.startsWith('"') && value.endsWith('"')) || (value.startsWith("'") && value.endsWith("'"))) {
			return value.slice(1, -1);
		}

		return value.replace(/\s+#.*$/, "");
	}

	return undefined;
}

/** Read `PROXY_API_KEY` from `~/.kiro-proxy/.env` — the proxy's own source of truth. */
function keyFromProxyEnvFile(): string | undefined {
	let text = "";
	try {
		text = readFileSync(join(homedir(), ".kiro-proxy", ".env"), "utf8");
	} catch {
		return undefined;
	}

	for (const line of text.split(/\r?\n/)) {
		const match = line.match(/^\s*(?:export\s+)?PROXY_API_KEY\s*=\s*(.+?)\s*$/);
		if (!match) continue;
		// First token only — the proxy accepts several space/comma-separated keys.
		return match[1].replace(/^["']|["']$/g, "").split(/[\s,]+/)[0];
	}
	return undefined;
}

export interface ProxyTarget {
	/** `<host>/v1` — the openai-completions base omp appends `/chat/completions` to. */
	baseUrl: string;
	/** Resolved bearer, or `undefined` when the proxy runs without auth. */
	apiKey: string | undefined;
}

/**
 * Resolve the proxy endpoint and publish its bearer under {@link API_KEY_ENV}.
 *
 * omp's `apiKey` field is resolved through the shared config resolver: `!cmd`
 * runs a shell command, anything else is looked up as an env var name before
 * falling back to a literal. Passing an *env var name* rather than the secret
 * itself sidesteps that ambiguity entirely, so a key that happens to start with
 * `!` can never be mistaken for a command.
 */
export function resolveProxy(agentDir: string): ProxyTarget {
	const rawUrl = readAgentEnvValue("KIRO_PROXY_URL", agentDir) || "http://127.0.0.1:8442";
	const baseUrl = `${rawUrl.trim().replace(/\/+$/, "").replace(/\/v1$/, "")}/v1`;

	const apiKey =
		readAgentEnvValue(API_KEY_ENV, agentDir) ||
		readAgentEnvValue("PROXY_API_KEY", agentDir) ||
		keyFromProxyEnvFile();
	if (apiKey) process.env[API_KEY_ENV] = apiKey;

	return { baseUrl, apiKey };
}

/**
 * Token limits Kiro serves, used only when the proxy is unreachable *and* omp has
 * no cached catalog yet. The proxy reports these live from Kiro's own
 * `ListAvailableModels` (`context_window` / `max_output_tokens` on `/v1/models`),
 * so this table is a cold-start floor rather than a source of truth.
 *
 * Verified against Kiro's catalog on 2026-08-18. The GPT-5.6 tiers match Kiro's
 * published 272K context window (https://kiro.dev/changelog/models/gpt-5-6/).
 */
const OFFLINE_LIMITS: Record<string, { contextWindow: number; maxTokens: number }> = {
	// The router dispatches to whichever model Kiro picks, so its own advertised
	// 1M window is an upper bound rather than something a turn can rely on. Both
	// numbers are therefore the floor across the catalog below: the smallest
	// window any backing model serves, and the smallest output ceiling. The proxy
	// deliberately reports no limits for this id, so this entry always applies.
	"auto-kiro": { contextWindow: 196_000, maxTokens: 64_000 },
	"claude-haiku-4.5": { contextWindow: 200_000, maxTokens: 64_000 },
	"claude-opus-4.5": { contextWindow: 200_000, maxTokens: 64_000 },
	"claude-opus-4.6": { contextWindow: 1_000_000, maxTokens: 64_000 },
	"claude-opus-4.7": { contextWindow: 1_000_000, maxTokens: 128_000 },
	"claude-opus-4.8": { contextWindow: 1_000_000, maxTokens: 128_000 },
	"claude-opus-5": { contextWindow: 1_000_000, maxTokens: 128_000 },
	"claude-sonnet-4": { contextWindow: 200_000, maxTokens: 64_000 },
	"claude-sonnet-4.5": { contextWindow: 200_000, maxTokens: 64_000 },
	"claude-sonnet-4.6": { contextWindow: 1_000_000, maxTokens: 64_000 },
	"claude-sonnet-5": { contextWindow: 1_000_000, maxTokens: 64_000 },
	"gpt-5.6-luna": { contextWindow: 272_000, maxTokens: 128_000 },
	"gpt-5.6-sol": { contextWindow: 272_000, maxTokens: 128_000 },
	"gpt-5.6-terra": { contextWindow: 272_000, maxTokens: 128_000 },
	"minimax-m2.1": { contextWindow: 196_000, maxTokens: 64_000 },
	"minimax-m2.5": { contextWindow: 196_000, maxTokens: 64_000 },
	"qwen3-coder-next": { contextWindow: 256_000, maxTokens: 64_000 },
};

/**
 * Cold-start limits for an id {@link OFFLINE_LIMITS} does not cover — typically a
 * model released after this table was last verified. Unknown non-Claude ids stay
 * deliberately conservative: omp compacts early instead of overflowing upstream.
 *
 * The Claude branch mirrors the proxy's own family rule
 * (`internal/model/resolver.go:familyContextLimit`): a 1M window for Opus and
 * Sonnet 4.x+, 200k otherwise.
 */
function fallbackLimits(id: string): { contextWindow: number; maxTokens: number } {
	const known = OFFLINE_LIMITS[id];
	if (known) return known;

	const claude = id.match(/claude-(opus|sonnet|haiku)-(\d+)/);
	if (!claude) return { contextWindow: 200_000, maxTokens: 8_192 };

	const [, family, major] = claude;
	const wide = (family === "opus" || family === "sonnet") && Number(major) >= 4;
	return {
		contextWindow: wide ? 1_000_000 : 200_000,
		maxTokens: family === "haiku" ? 16_384 : 32_000,
	};
}

/**
 * Effort ladder the proxy understands on the OpenAI surface: it maps
 * `reasoning_effort` to a thinking budget as a percentage of max output tokens
 * (`internal/api/openai/convert.go:reasoningEffortBudget`). `max` is not one of
 * its rungs, so it is remapped onto `xhigh` rather than advertised.
 *
 * `Effort` is a cross-package `const enum`; importing it would emit a runtime
 * dependency on the host catalog, so its wire strings are asserted once here.
 */
const THINKING = {
	mode: "effort",
	efforts: ["minimal", "low", "medium", "high", "xhigh"],
	defaultLevel: "high",
	effortMap: { max: "xhigh" },
} as unknown as NonNullable<ProviderModelConfig["thinking"]>;

/**
 * One entry of the proxy's `/v1/models` listing.
 *
 * `contextWindow` and `maxOutputTokens` are the proxy's private extension to the
 * OpenAI shape, carrying the token limits Kiro reports for the model. They are
 * absent when the proxy could not establish a limit, which is why they are
 * optional here rather than defaulted — an absent field means "ask the fallback",
 * not "zero".
 */
export interface KiroModel {
	id: string;
	contextWindow?: number;
	maxOutputTokens?: number;
}

/**
 * Accept a proxy-supplied limit only when it is a usable token count. A
 * malformed, zero, negative or non-integer value is discarded so the offline
 * fallback answers instead of omp inheriting a broken window.
 */
function tokenLimit(value: unknown): number | undefined {
	return typeof value === "number" && Number.isInteger(value) && value > 0 ? value : undefined;
}

/**
 * Turn a discovered entry into an omp model definition. Token limits come from
 * the proxy when it supplied them (Kiro's own `tokenLimits`), otherwise from
 * {@link fallbackLimits}. Everything else — modality, effort ladder, credit-based
 * cost — is a property of the model family, not of the catalog.
 */
export function buildModel(entry: KiroModel): ProviderModelConfig {
	const { id } = entry;
	const claude = id.includes("claude") || id === "auto-kiro";
	const qwen = id.startsWith("qwen");
	const reasoning = !qwen;
	const fallback = fallbackLimits(id);

	return {
		id,
		name: `Kiro ${id}`,
		reasoning,
		...(reasoning ? { thinking: THINKING } : {}),
		input: (claude ? ["text", "image"] : ["text"]) as ("text" | "image")[],
		// Billed in Kiro credits, not per token.
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
		contextWindow: tokenLimit(entry.contextWindow) ?? fallback.contextWindow,
		maxTokens: tokenLimit(entry.maxOutputTokens) ?? fallback.maxTokens,
	};
}

/** Live catalog from the proxy; falls back to the seed when it is unreachable. */
export async function fetchModels(baseUrl: string, apiKey: string | undefined): Promise<KiroModel[]> {
	try {
		const response = await fetch(`${baseUrl}/models`, {
			headers: apiKey ? { authorization: `Bearer ${apiKey}` } : undefined,
			signal: AbortSignal.timeout(5_000),
		});
		if (!response.ok) throw new Error(`GET /v1/models -> ${response.status}`);
		const payload = (await response.json()) as {
			data?: { id?: string; context_window?: unknown; max_output_tokens?: unknown }[];
		};
		const models = (payload.data ?? [])
			.filter((entry): entry is { id: string; context_window?: unknown; max_output_tokens?: unknown } =>
				Boolean(entry?.id),
			)
			.map((entry) => ({
				id: entry.id,
				contextWindow: tokenLimit(entry.context_window),
				maxOutputTokens: tokenLimit(entry.max_output_tokens),
			}));
		return models.length ? models : SEED_MODELS;
	} catch {
		return SEED_MODELS;
	}
}

/** Credit balance the proxy reports for the account it is currently serving. */
export interface KiroCredits {
	total: number;
	used: number;
	remaining: number;
}

/**
 * Live credit balance from the proxy's `GET /v1/usage`.
 *
 * Unlike {@link fetchModelIds} this throws instead of degrading: the status line
 * swallows the failure, but `/kiro-credits` reports the reason.
 */
export async function fetchCredits(baseUrl: string, apiKey: string | undefined): Promise<KiroCredits> {
	const response = await fetch(`${baseUrl}/usage`, {
		headers: apiKey ? { authorization: `Bearer ${apiKey}` } : undefined,
		signal: AbortSignal.timeout(5_000),
	});
	const payload = (await response.json().catch(() => undefined)) as
		| { total_credits?: number; used_credits?: number; remaining_credits?: number; error?: { message?: string } }
		| undefined;
	if (!response.ok) {
		// The proxy emits an OpenAI-shaped envelope: {"error":{"message":"…"}}
		// (internal/api/openai/routes.go:writeError).
		throw new Error(payload?.error?.message ?? `GET /v1/usage -> ${response.status}`);
	}
	const { total_credits: total, used_credits: used, remaining_credits: remaining } = payload ?? {};
	if (typeof total !== "number" || typeof used !== "number" || typeof remaining !== "number") {
		throw new Error("GET /v1/usage returned an unrecognized payload");
	}
	return { total, used, remaining };
}
