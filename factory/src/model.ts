// The model is reached through the kodo-inference AI gateway, which speaks
// OpenAI Chat Completions. Each gateway route ("agent", "default", "sim") is
// registered as a model whose x-ai-eg-model header selects the route, the
// same way the Go harness calls it.
import { createModels, createProvider, type Model, type MutableModels } from "@earendil-works/pi-ai";
import { openAICompletionsApi } from "@earendil-works/pi-ai/api/openai-completions.lazy";

export const PROVIDER = "kodo";

export type GatewayOptions = {
	baseUrl: string;
	keyHeader: string;
	key: string;
	routes: readonly string[];
};

export function gatewayModel(route: string, baseUrl: string): Model<"openai-completions"> {
	return {
		id: route,
		name: `gateway route ${route}`,
		api: "openai-completions",
		provider: PROVIDER,
		baseUrl,
		reasoning: false,
		input: ["text"],
		// The gateway picks the upstream model; cost is not known here.
		cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
		contextWindow: 200_000,
		maxTokens: 32_000,
		headers: { "x-ai-eg-model": route },
		compat: { supportsStore: false, supportsDeveloperRole: false },
	};
}

export function gatewayModels(opts: GatewayOptions): MutableModels {
	const provider = createProvider({
		id: PROVIDER,
		name: "kodo-inference gateway",
		baseUrl: opts.baseUrl,
		auth: {
			apiKey: {
				name: "kodo gateway key",
				resolve: async () => ({
					auth: { apiKey: opts.key, headers: { [opts.keyHeader]: opts.key } },
					source: "FACTORY_MODEL_KEY_FILE",
				}),
			},
		},
		models: opts.routes.map((r) => gatewayModel(r, opts.baseUrl)),
		api: openAICompletionsApi(),
	});
	const models = createModels();
	models.setProvider(provider);
	return models;
}
