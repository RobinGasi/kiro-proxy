/**
 * kiro model provider — run Kiro's models under **omp's own agent loop**: omp's
 * tools, skills, rules, approval gate, checkpointing, subagents and system
 * prompt, with Kiro supplying the inference.
 *
 * Wire path: omp → local `kiro-proxy` (`/v1/chat/completions`) → Kiro backend.
 * No AWS Bedrock adapter, no vendor SDK, no cloud API key.
 *
 * Config (env or `~/.omp/agent/.env`):
 *   - KIRO_PROXY_URL      Proxy base. Default `http://127.0.0.1:8442`.
 *   - KIRO_PROXY_API_KEY  Bearer. Falls back to `PROXY_API_KEY`, then
 *                         `~/.kiro-proxy/.env`.
 */

import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";
import { API_KEY_ENV, buildModel, fetchModelIds, resolveProxy } from "./proxy.ts";

const PROVIDER = "kiro";

export default function kiroProviderExtension(pi: ExtensionAPI) {
	const { baseUrl, apiKey } = resolveProxy(pi.pi.getAgentDir());

	pi.setLabel("Kiro models");

	pi.registerProvider(PROVIDER, {
		baseUrl,
		// An env var *name*, not the secret: omp resolves `apiKey` through the
		// shared config resolver, and `resolveProxy` publishes the key there.
		...(apiKey ? { apiKey: API_KEY_ENV } : {}),
		api: "openai-completions",
		authHeader: true,
		// Runs through omp's SQLite model cache (24 h TTL), so the catalog tracks
		// whatever the proxy currently serves instead of a hardcoded list.
		fetchDynamicModels: async (resolvedKey) => (await fetchModelIds(baseUrl, resolvedKey ?? apiKey)).map(buildModel),
	});
}
