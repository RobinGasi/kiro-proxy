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

/**
 * Fallback catalog, used only when the proxy is unreachable *and* omp has no
 * cached model list yet. Keeping it small and current beats an empty provider.
 */
const SEED_MODEL_IDS = [
	"auto-kiro",
	"claude-opus-5",
	"claude-sonnet-5",
	"claude-opus-4.8",
	"claude-sonnet-4.6",
	"claude-haiku-4.5",
	"gpt-5.6-sol",
];

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
 * Input-context window Kiro actually serves, mirroring the proxy's own
 * `internal/model/resolver.go:ContextLimitFor`: a 1M window for Opus and Sonnet
 * 4.x+, 200k for Haiku 4.5. Unknown Claude ids — and the `auto-kiro` router,
 * whose backing model cannot be known ahead of time — fall back to 200k so omp
 * compacts early rather than overflowing.
 */
function claudeContextWindow(id: string): number {
	const match = id.match(/claude-(opus|sonnet|haiku)-(\d+)/);
	if (!match) return 200_000;
	const family = match[1];
	const major = Number(match[2]);
	return (family === "opus" || family === "sonnet") && major >= 4 ? 1_000_000 : 200_000;
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

/** The proxy's `/v1/models` returns bare ids, so per-family metadata lives here. */
export function buildModel(id: string): ProviderModelConfig {
	const claude = id.includes("claude") || id === "auto-kiro";
	const qwen = id.startsWith("qwen");
	const reasoning = !qwen;

	return {
		id,
		name: `Kiro ${id}`,
		reasoning,
		...(reasoning ? { thinking: THINKING } : {}),
		input: (claude ? ["text", "image"] : ["text"]) as ("text" | "image")[],
		// Billed in Kiro credits, not per token.
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
		contextWindow: claude ? claudeContextWindow(id) : id.startsWith("minimax") ? 200_000 : 128_000,
		maxTokens: claude ? (id.includes("haiku") ? 16_384 : 32_000) : 8_192,
	};
}

/** Live model list from the proxy; falls back to the seed when it is unreachable. */
export async function fetchModelIds(baseUrl: string, apiKey: string | undefined): Promise<string[]> {
	try {
		const response = await fetch(`${baseUrl}/models`, {
			headers: apiKey ? { authorization: `Bearer ${apiKey}` } : undefined,
			signal: AbortSignal.timeout(5_000),
		});
		if (!response.ok) throw new Error(`GET /v1/models -> ${response.status}`);
		const payload = (await response.json()) as { data?: { id?: string }[] };
		const ids = (payload.data ?? []).map((model) => model.id).filter((id): id is string => Boolean(id));
		return ids.length ? ids : SEED_MODEL_IDS;
	} catch {
		return SEED_MODEL_IDS;
	}
}
