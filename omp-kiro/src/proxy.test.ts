import { describe, expect, test } from "bun:test";
import { buildModel, fetchModels } from "./proxy.ts";

describe("buildModel token limits", () => {
	test("prefers the limits the proxy reports", () => {
		// The proxy relays Kiro's own `tokenLimits`, so a re-tiered model is picked
		// up without touching the offline table.
		const model = buildModel({ id: "gpt-5.6-sol", contextWindow: 400_000, maxOutputTokens: 200_000 });
		expect(model.contextWindow).toBe(400_000);
		expect(model.maxTokens).toBe(200_000);
	});

	test("falls back to Kiro's published limits when the proxy reports none", () => {
		// Cold cache with the proxy down: Sol must still advertise the 272K window
		// Kiro documents, not a generic 128K default.
		const sol = buildModel({ id: "gpt-5.6-sol" });
		expect(sol.contextWindow).toBe(272_000);
		expect(sol.maxTokens).toBe(128_000);

		for (const id of ["gpt-5.6-terra", "gpt-5.6-luna"]) {
			expect(buildModel({ id }).contextWindow).toBe(272_000);
		}
		expect(buildModel({ id: "qwen3-coder-next" }).contextWindow).toBe(256_000);
		expect(buildModel({ id: "minimax-m2.5" }).contextWindow).toBe(196_000);
	});

	test("rejects malformed limits rather than inheriting them", () => {
		for (const bad of [0, -1, 1.5, Number.NaN, Number.POSITIVE_INFINITY]) {
			const model = buildModel({ id: "gpt-5.6-sol", contextWindow: bad, maxOutputTokens: bad });
			expect(model.contextWindow).toBe(272_000);
			expect(model.maxTokens).toBe(128_000);
		}
		// Non-numeric shapes reach here from untyped JSON.
		const wrongType = buildModel({
			id: "gpt-5.6-sol",
			contextWindow: "272000" as unknown as number,
			maxOutputTokens: null as unknown as number,
		});
		expect(wrongType.contextWindow).toBe(272_000);
		expect(wrongType.maxTokens).toBe(128_000);
	});

	test("keeps the conservative fallbacks for unknown ids", () => {
		// An id absent from the offline table and from the Claude family rule: omp
		// compacts early instead of overflowing upstream.
		const unknown = buildModel({ id: "mystery-model" });
		expect(unknown.contextWindow).toBe(200_000);
		expect(unknown.maxTokens).toBe(8_192);

		// The router's backing model cannot be known ahead of time, so it gets the
		// floor across the catalog rather than any one model's limits. The proxy
		// reports no limits for it, so this path is the live behaviour, not just a
		// cold-start one.
		const router = buildModel({ id: "auto-kiro" });
		expect(router.contextWindow).toBe(196_000);
		expect(router.maxTokens).toBe(64_000);

		// An unreleased Claude still gets its family's window.
		const future = buildModel({ id: "claude-opus-9" });
		expect(future.contextWindow).toBe(1_000_000);
		expect(future.maxTokens).toBe(32_000);
		expect(buildModel({ id: "claude-haiku-9" }).contextWindow).toBe(200_000);
	});

	test("leaves family metadata driven by the id, not the catalog", () => {
		const sol = buildModel({ id: "gpt-5.6-sol", contextWindow: 272_000 });
		expect(sol.input).toEqual(["text"]);
		expect(sol.reasoning).toBe(true);
		expect(sol.cost).toEqual({ input: 0, output: 0, cacheRead: 0, cacheWrite: 0 });

		const opus = buildModel({ id: "claude-opus-5" });
		expect(opus.input).toEqual(["text", "image"]);

		// Qwen has no thinking ladder on this surface.
		const qwen = buildModel({ id: "qwen3-coder-next" });
		expect(qwen.reasoning).toBe(false);
		expect(qwen.thinking).toBeUndefined();
	});
});

describe("fetchModels", () => {
	/** Serve one `/v1/models` body on an ephemeral port and return its base URL. */
	async function serving(body: unknown, status = 200) {
		const server = Bun.serve({
			port: 0,
			fetch: () => new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } }),
		});
		return { baseUrl: `http://127.0.0.1:${server.port}/v1`, stop: () => server.stop(true) };
	}

	test("maps the proxy's snake_case capability fields", async () => {
		const { baseUrl, stop } = await serving({
			object: "list",
			data: [
				{ id: "gpt-5.6-sol", object: "model", context_window: 272_000, max_output_tokens: 128_000 },
				{ id: "mystery-model", object: "model" },
			],
		});
		try {
			expect(await fetchModels(baseUrl, undefined)).toEqual([
				{ id: "gpt-5.6-sol", contextWindow: 272_000, maxOutputTokens: 128_000 },
				{ id: "mystery-model", contextWindow: undefined, maxOutputTokens: undefined },
			]);
		} finally {
			stop();
		}
	});

	// A rejected fetch is what makes omp retry in minutes; a resolved list is
	// cached as the catalog for 24 h.
	test("rejects when the proxy errors or lists nothing", async () => {
		const failing = await serving({ error: { message: "no accounts" } }, 503);
		try {
			await expect(fetchModels(failing.baseUrl, undefined)).rejects.toThrow("503");
		} finally {
			failing.stop();
		}

		const empty = await serving({ object: "list", data: [] });
		try {
			await expect(fetchModels(empty.baseUrl, undefined)).rejects.toThrow("no models");
		} finally {
			empty.stop();
		}
	});

	test("rejects when the proxy is unreachable", async () => {
		// Nothing is listening on this port, so the fetch rejects.
		await expect(fetchModels("http://127.0.0.1:1/v1", undefined)).rejects.toThrow();
	});
});
