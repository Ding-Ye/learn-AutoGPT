# Source: classic/forge/forge/llm/providers/multi.py
#         classic/forge/forge/llm/providers/openai.py
# Upstream URLs:
#   https://github.com/Significant-Gravitas/AutoGPT/blob/master/classic/forge/forge/llm/providers/multi.py
#   https://github.com/Significant-Gravitas/AutoGPT/blob/master/classic/forge/forge/llm/providers/openai.py
# License: MIT (the classic/ subtree is MIT-licensed; only autogpt_platform/
# is under Polyform Shield 1.0).
#
# This file pulls the multi-provider abstraction (and a tiny snippet of
# the OpenAI provider) into one annotated reading for s03 of learn-AutoGPT.
# It shows how AutoGPT classic puts four distinct backends behind a
# single `MultiProvider` and lazy-initializes each on first use.
#
# Lines marked [→ sNN] indicate which Go session in this repo teaches the
# corresponding upstream concept. Generic typing (TypeVar/ParamSpec),
# logging, and Pydantic settings boilerplate are stripped where they
# distract from the routing structure.


# ─────────────────────────────────────────────────────────────────────────
# llm/providers/multi.py · MultiProvider — the aggregator
# ─────────────────────────────────────────────────────────────────────────

from .anthropic import ANTHROPIC_CHAT_MODELS, AnthropicProvider
from .groq import GROQ_CHAT_MODELS, GroqProvider
from .llamafile import LLAMAFILE_CHAT_MODELS, LlamafileProvider
from .openai import OPEN_AI_CHAT_MODELS, OpenAIProvider

# CHAT_MODELS aggregates every backend's models into a single dict.
# Each value is a ChatModelInfo carrying `provider_name` so the router
# knows which provider owns the model.
# [→ s03: our Go version doesn't keep this dict — instead, the user
#   picks the backend explicitly via the -provider flag in main.go,
#   because 7/8 OpenAI-compat backends share one OpenAIProvider.]
CHAT_MODELS = {
    **ANTHROPIC_CHAT_MODELS,
    **GROQ_CHAT_MODELS,
    **LLAMAFILE_CHAT_MODELS,
    **OPEN_AI_CHAT_MODELS,
}


class MultiProvider(BaseChatModelProvider):
    """One Provider that routes calls to the appropriate backend by model name.

    AutoGPT lazy-instantiates each provider the first time it's needed,
    caching the instance so subsequent calls reuse the same client. In
    Python this matters because OpenAI/Anthropic SDK constructors are
    non-trivial (credential probing, retry configuration, etc).
    [→ s03: our Go version skips this cache — `NewAnthropicProvider(...)`
    is a struct literal + one *http.Client, microseconds in cost.]
    """

    # Lazy cache. Maps ModelProviderName ("anthropic" | "openai" | ...) to
    # the constructed provider instance.
    _provider_instances: dict[ModelProviderName, ChatModelProvider]

    def __init__(self, settings=None, logger=None):
        super().__init__(settings=settings, logger=logger)
        self._budget = self._settings.budget or ModelProviderBudget()
        self._provider_instances = {}  # populated on first _get_provider call

    async def create_chat_completion(
        self,
        model_prompt: list[ChatMessage],
        model_name: ModelName,
        completion_parser: Callable[[AssistantChatMessage], _T] = lambda _: None,
        functions: Optional[list[CompletionModelFunction]] = None,
        max_output_tokens: Optional[int] = None,
        prefill_response: str = "",
        **kwargs,
    ) -> ChatModelResponse[_T]:
        """The single entry point — routes to whichever provider owns model_name.

        [→ s03: this is exactly Provider.CreateMessage in Go. We don't take
        completion_parser (that lives in s04 strategy.ParseResponse) or
        prefill_response (Anthropic native tool_use makes prefill obsolete).]
        """
        return await self.get_model_provider(model_name).create_chat_completion(
            model_prompt=model_prompt,
            model_name=model_name,
            completion_parser=completion_parser,
            functions=functions,
            max_output_tokens=max_output_tokens,
            prefill_response=prefill_response,
            **kwargs,
        )

    def get_model_provider(self, model: ModelName) -> ChatModelProvider:
        """model_name → provider_name → cached instance.

        Two indirections: model_info.provider_name from the global dict,
        then _get_provider for the lazy cache.
        [→ s03: in Go, main.go switches directly on -provider flag —
        only one indirection (provider name → constructor call).]
        """
        model_info = CHAT_MODELS[model]
        return self._get_provider(model_info.provider_name)

    def _get_provider(self, provider_name: ModelProviderName) -> ChatModelProvider:
        """Lazy init. The first call for a given provider builds the instance,
        loads credentials from env via Pydantic settings, and stores it in
        _provider_instances. Subsequent calls return the cached instance.

        [→ s03: this whole method has no Go analog — we construct providers
        eagerly in main.go. The Python rationale is that OpenAI/Anthropic
        SDK clients are non-trivial to construct (credential probing,
        token detection, retry config). Go's *http.Client is zero-cost.]
        """
        _provider = self._provider_instances.get(provider_name)
        if not _provider:
            Provider = self._get_provider_class(provider_name)
            settings = Provider.default_settings.model_copy(deep=True)
            settings.budget = self._budget
            settings.configuration.extra_request_headers.update(
                self._settings.configuration.extra_request_headers
            )
            # Credential loading from env vars via Pydantic BaseSettings.
            # [→ s03: in Go, main.go reads `os.Getenv(prof.APIKey)` directly
            # from the providerProfiles table. No Pydantic.]
            if settings.credentials is None:
                credentials_field = settings.model_fields["credentials"]
                Credentials = get_args(credentials_field.annotation)[0]
                try:
                    settings.credentials = Credentials.from_env()
                except ValidationError as e:
                    if credentials_field.is_required():
                        raise ValueError(
                            f"{Provider.__name__} is unavailable: "
                            "can't load credentials"
                        ) from e
            self._provider_instances[provider_name] = _provider = Provider(
                settings=settings, logger=self._logger,
            )
            _provider._budget = self._budget
        return _provider

    @classmethod
    def _get_provider_class(cls, provider_name: ModelProviderName):
        """provider_name → class (hard-coded table).

        [→ s03: our Go equivalent is the switch statement in main.go's
        provider construction, which collapses 7 OpenAI-compat backends
        into a single default case (NewOpenAIProvider).]
        """
        try:
            return {
                ModelProviderName.ANTHROPIC: AnthropicProvider,
                ModelProviderName.GROQ: GroqProvider,
                ModelProviderName.LLAMAFILE: LlamafileProvider,
                ModelProviderName.OPENAI: OpenAIProvider,
            }[provider_name]
        except KeyError:
            raise ValueError(f"{provider_name} is not a known provider") from None


# ─────────────────────────────────────────────────────────────────────────
# llm/providers/openai.py · OpenAIProvider — concrete provider (snippet)
# ─────────────────────────────────────────────────────────────────────────

class OpenAIProvider(
    BaseOpenAIChatProvider[OpenAIModelName, OpenAISettings],
    BaseOpenAIEmbeddingProvider[OpenAIModelName, OpenAISettings],
):
    """Native OpenAI/Azure-OpenAI client. Translation of ChatMessage to
    the SDK's ChatCompletionMessageParam happens in the parent class
    BaseOpenAIChatProvider._get_chat_completion_args (not shown here).

    [→ s03: our Go OpenAIProvider does the translation in one file — see
    translateRequestToOpenAI / translateResponseFromOpenAI in
    provider_openai.go. ~80 lines, single function, no inheritance.]
    """
    MODELS = OPEN_AI_MODELS
    CHAT_MODELS = OPEN_AI_CHAT_MODELS
    EMBEDDING_MODELS = OPEN_AI_EMBEDDING_MODELS

    def __init__(self, settings=None, logger=None):
        super().__init__(settings=settings, logger=logger)
        # Azure vs OpenAI proper — the upstream client construction
        # already non-trivial enough to motivate the lazy cache above.
        if self._credentials.api_type == SecretStr("azure"):
            from openai import AsyncAzureOpenAI
            self._client = AsyncAzureOpenAI(
                **self._credentials.get_api_access_kwargs()
            )
        else:
            from openai import AsyncOpenAI
            self._client = AsyncOpenAI(
                **self._credentials.get_api_access_kwargs()
            )


# ─────────────────────────────────────────────────────────────────────────
# Reading map — which session of learn-AutoGPT teaches each upstream symbol
# ─────────────────────────────────────────────────────────────────────────
#
# MultiProvider class                 → s03 (this is the central comparison —
#                                            multi-backend abstraction; Go's
#                                            version is per-backend constructor)
# MultiProvider._provider_instances   → s03 (Go skips this — zero-init structs
#                                            cost nothing, no need to cache)
# MultiProvider.get_model_provider    → s03 (Go uses explicit -provider flag;
#                                            no model_name → provider_name
#                                            indirection)
# MultiProvider._get_provider_class   → s03 (Go's switch on *provider in main.go)
# CHAT_MODELS dict                    → s03 (we don't aggregate — each backend
#                                            self-identifies via -provider flag)
# OpenAIProvider native SDK use       → s03 (Go reaches HTTP directly with
#                                            net/http; no SDK dependency)
# BaseOpenAIChatProvider              → s03 (no analog; our translation lives
#                                            in one file, no inheritance)
# _functions_compat_fix_kwargs        → s04 (degradation path for non-tool-call
#                                            models — we leave this out of s03
#                                            since modern backends all support
#                                            tool_calls)
# litellm wrapper for OpenRouter      → s03 (Go skips — OpenRouter speaks
#                                            OpenAI-compat directly, no router
#                                            library needed)
# ChatModelResponse[T] generic        → s04 (Generic completion_parser is
#                                            replaced by PromptStrategy.ParseResponse)
