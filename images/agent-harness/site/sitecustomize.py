"""agent-server's start-up hook: agent-run puts this directory first on agent-server's PYTHONPATH
(server_env), and Python imports sitecustomize from it before anything else.

F29: a backend's `ping` reaches agent-server through agent-router with its numeric id unrewritten,
so agent-router answers the client's reply 400 "invalid response ID type" (envoyproxy/ai-gateway#2715).
mcp 1.28.1 sends a reply inline from StreamableHTTPTransport.post_writer: _handle_post_request's
raise_for_status() ends post_writer, which closes both session streams, while fastmcp's
is_connected() stays True. Every later tool call then fails with an empty ClosedResourceError and
the SDK's reconnect-once never fires. A refused reply costs the run nothing, so it is logged and
dropped; requests and notifications are untouched. Belt and braces: agent-router is being fixed to
answer client replies 202 itself (F29's gateway half); the harness does not depend on it.

#99: one transient model error must not end the run; see patch_model_retry.

cloud-native-ref#2283: each MCP request carries the traceparent of the span that sent it; see
patch_traceparent.

#187: a run conversation loads no repository or user plugin; see patch_ambient_plugins.

Internal runs' Anthropic prompt caching reaches the gateway in the field it converts; see
cache_breakpoints.
"""
import asyncio
import contextvars
import logging
import os
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
log = logging.getLogger("agent-harness.mcp")


def patch(transport, httpx, replies) -> bool:
    """Wraps transport._handle_post_request so a 4xx to one of `replies` is logged, not raised."""
    original = getattr(transport, "_handle_post_request", None)
    if original is None:
        print("agent-harness: F29 patch NOT applied: %s has no _handle_post_request (an mcp upgrade?); "
              "a refused reply to a server ping will end the MCP session" % transport.__name__,
              file=sys.stderr, flush=True)
        return False

    async def _handle_post_request(self, ctx):
        try:
            await original(self, ctx)
        except httpx.HTTPStatusError as exc:
            if not (isinstance(ctx.session_message.message.root, replies) and 400 <= exc.response.status_code < 500):
                raise
            log.warning("F29: the MCP server refused this client's reply to its request (HTTP %d); "
                        "the session is kept", exc.response.status_code)

    transport._handle_post_request = _handle_post_request
    return True


# The traceparent of the message _handle_post_request is sending, for _prepare_headers to add.
_SENDING = contextvars.ContextVar("agent_harness_traceparent", default=None)


def current_traceparent(trace) -> str | None:
    sc = trace.get_current_span().get_span_context()
    return "00-%032x-%016x-01" % (sc.trace_id, sc.span_id) if sc.is_valid else None


def patch_traceparent(session_module, transport, trace) -> bool:
    """Sends each MCP request with the traceparent of the span current where it was made.

    mcp builds the HTTP headers from the transport's config, once per session, and sends from its
    own post_writer task, where the caller's span is no longer current. So the span is read when
    the message is made (in the caller, where MCPToolExecutor.call_tool's @observe made it current)
    and the header added when it is posted. Without a current span the config's header stays: the
    run span agent-run puts there."""
    base = getattr(session_module, "SessionMessage", None)
    post = getattr(transport, "_handle_post_request", None)
    prepare = getattr(transport, "_prepare_headers", None)
    if base is None or post is None or prepare is None:
        print("agent-harness: #2283 traceparent NOT applied: no SessionMessage, _handle_post_request or "
              "_prepare_headers (an mcp upgrade?); MCP spans nest under the run, not their step",
              file=sys.stderr, flush=True)
        return False

    class TracedSessionMessage(base):
        def __init__(self, *args, **kwargs):
            super().__init__(*args, **kwargs)
            self.traceparent = current_traceparent(trace)

    async def _handle_post_request(self, ctx):
        token = _SENDING.set(getattr(ctx.session_message, "traceparent", None))
        try:
            await post(self, ctx)
        finally:
            _SENDING.reset(token)

    def _prepare_headers(self):
        headers = prepare(self)
        if _SENDING.get():
            headers["traceparent"] = _SENDING.get()
        return headers

    session_module.SessionMessage = TracedSessionMessage
    transport._handle_post_request = _handle_post_request
    transport._prepare_headers = _prepare_headers
    return True


# OD-7: three attempts, 10 s then 20 s apart: 30 s of backoff, inside the 60 s bound.
RETRY_WAITS_S = (10, 20)
# Envoy's body when it cannot reach the provider. The status depends on which hop answers, so the
# text decides.
ENVOY_RESET = "upstream connect error or disconnect/reset before headers"


def transient(exc, openai) -> bool:
    """A 503, or a connection that failed before any token: safe to send again. Never a 429, which can
    be the gateway's own budget throttle (OD-7), and never a timeout, which would outlast the bound.

    The whole cause chain is read: litellm 1.93 reports a refused or reset connection as a 500
    InternalServerError, with openai's APIConnectionError only as its cause."""
    chain = []
    while exc is not None and len(chain) < 10:
        chain.append(exc)
        exc = exc.__cause__ or exc.__context__
    if any(isinstance(e, (openai.RateLimitError, openai.APITimeoutError)) or getattr(e, "status_code", None) == 429
           for e in chain):
        return False
    return any(getattr(e, "status_code", None) == 503 or isinstance(e, openai.APIConnectionError)
               or ENVOY_RESET in str(e) for e in chain)


def _log_retry(line: str) -> None:
    """Prints a retry and counts it in $AGENT_RUN_RETRY_LOG."""
    print(line, file=sys.stderr, flush=True)
    try:
        with open(os.environ["AGENT_RUN_RETRY_LOG"], "a") as f:
            f.write(line + "\n")
    except (KeyError, OSError):
        pass  # the count is best effort, the retry is not


def _retrying(attempt, wait, exc, kwargs, openai) -> bool:
    """Whether to send the call again; if so, logs the retry and counts it in $AGENT_RUN_RETRY_LOG."""
    # A stream that failed midway has already produced tokens.
    if wait is None or kwargs.get("enable_streaming") or not transient(exc, openai):
        return False
    _log_retry("agent-run model retry %d/%d in %ds: %s %s" % (
        attempt, len(RETRY_WAITS_S), wait, type(exc).__name__, " ".join(str(exc).split())[:200]))
    return True


def patch_model_retry(llm, openai, sleep=time.sleep, asleep=asyncio.sleep) -> bool:
    """Wraps one model call, sync and async (agent-server's own path), to retry a transient error
    (#99, OD-7).

    The SDK's own retry (num_retries) cannot leave a 429 out, so agent-run sets it to 0. litellm's
    OpenAI client also retried twice of its own, a 429 included, within 1.5 s: too fast for a
    provider blip and against OD-7, so max_retries=0 turns it off.

    SDK 1.56 bounds each async model call by LLM.timeout (300 s by default): the attempts and their
    waits share that window, so a slow failure near it loses its retry (fast 503 bursts still retry)."""
    sync_call = getattr(llm, "_transport_call", None)
    async_call = getattr(llm, "_atransport_call", None)
    if sync_call is None or async_call is None:
        print("agent-harness: #99 retry NOT applied: %s has no _transport_call or _atransport_call (an SDK "
              "upgrade?); one transient model error will end the run" % llm.__name__, file=sys.stderr, flush=True)
        return False

    def _transport_call(self, **kwargs):
        kwargs["max_retries"] = 0
        for attempt, wait in enumerate((*RETRY_WAITS_S, None), 1):
            try:
                return sync_call(self, **kwargs)
            except Exception as exc:
                if not _retrying(attempt, wait, exc, kwargs, openai):
                    raise
            sleep(wait)

    async def _atransport_call(self, **kwargs):
        kwargs["max_retries"] = 0
        for attempt, wait in enumerate((*RETRY_WAITS_S, None), 1):
            try:
                return await async_call(self, **kwargs)
            except Exception as exc:
                if not _retrying(attempt, wait, exc, kwargs, openai):
                    raise
            await asleep(wait)

    llm._transport_call = _transport_call
    llm._atransport_call = _atransport_call
    return True


def patch_timeout_retry(llm, timeout_error) -> bool:
    """Wraps the model call's public entry points, sync and async, to send a call that hit the SDK's hard
    timeout (LLMTimeoutError, 300 s) once more, as a fresh request with a fresh window.

    Live on gcp-0 (2026-10-27) a rare call hung on glm-5.3 via Z.ai (p99 was 82 s) and one timeout failed
    the whole run, twice. patch_model_retry cannot help: the SDK's window wraps its attempts, so a hang
    has no time left to retry inside it. Only that timeout is retried, once: never a 4xx or any other
    error, and never a call that already emitted a token to its stream callback."""
    sync_call = getattr(llm, "completion", None)
    async_call = getattr(llm, "agenerate", None)
    if sync_call is None or async_call is None:
        print("agent-harness: timeout retry NOT applied: %s has no completion or agenerate (an SDK upgrade?); "
              "one model timeout will end the run" % getattr(llm, "__name__", llm), file=sys.stderr, flush=True)
        return False

    def attempt_kwargs(kwargs):
        """kwargs for one attempt, with on_token wrapped to note that a token came out. SDK 1.56 always
        passes a stream callback, so its presence says nothing: only a token already emitted does."""
        seen = [False]
        out = dict(kwargs)
        cb = kwargs.get("on_token")
        if cb is not None:
            def on_token(*args, **kw):
                seen[0] = True
                return cb(*args, **kw)

            out["on_token"] = on_token
        return out, seen

    def again(exc, seen) -> bool:
        if seen[0]:
            return False
        _log_retry("agent-run model retry timeout 1/1: %s %s" % (type(exc).__name__, " ".join(str(exc).split())[:200]))
        return True

    def completion(self, *args, **kwargs):
        kw, seen = attempt_kwargs(kwargs)
        try:
            return sync_call(self, *args, **kw)
        except timeout_error as exc:
            if not again(exc, seen):
                raise
        return sync_call(self, *args, **attempt_kwargs(kwargs)[0])

    async def agenerate(self, *args, **kwargs):
        kw, seen = attempt_kwargs(kwargs)
        try:
            return await async_call(self, *args, **kw)
        except timeout_error as exc:
            if not again(exc, seen):
                raise
        return await async_call(self, *args, **attempt_kwargs(kwargs)[0])

    llm.completion = completion
    llm.agenerate = agenerate
    return True


BREAKPOINT = {"mode": "explicit"}


def cache_breakpoints(messages: list) -> None:
    """Rewrites the SDK's Anthropic `cache_control` markers, in place, as the OpenAI text-part field
    `prompt_cache_breakpoint`: the only marker agentgateway 1.6 carries from Chat Completions to
    Anthropic Messages; it drops `cache_control`. The SDK puts a tool message's marker on the
    message (litellm#6422), and 1.6 reads it only on a part, so it moves to the last one."""
    for message in messages:
        marked = message.pop("cache_control", None) is not None
        content = message.get("content")
        if marked and isinstance(content, str):
            content = message["content"] = [{"type": "text", "text": content}]
        if not isinstance(content, list):
            continue
        for part in content:
            if isinstance(part, dict) and part.pop("cache_control", None) is not None:
                part["prompt_cache_breakpoint"] = BREAKPOINT
        if marked and content and isinstance(content[-1], dict):
            content[-1]["prompt_cache_breakpoint"] = BREAKPOINT


def patch_cache_breakpoints(llm) -> bool:
    """Wraps both model calls so the SDK's cache markers reach the gateway in its field. Only an
    internal run has markers (agent_run's supports_prompt_cache), so a public call is untouched."""
    sync_call = getattr(llm, "_transport_call", None)
    async_call = getattr(llm, "_atransport_call", None)
    if sync_call is None or async_call is None:
        print("agent-harness: prompt cache NOT applied: %s has no _transport_call or _atransport_call (an "
              "SDK upgrade?); internal runs pay full input price every turn" % llm.__name__,
              file=sys.stderr, flush=True)
        return False

    def _transport_call(self, *, messages, **kwargs):
        cache_breakpoints(messages)
        return sync_call(self, messages=messages, **kwargs)

    async def _atransport_call(self, *, messages, **kwargs):
        cache_breakpoints(messages)
        return await async_call(self, messages=messages, **kwargs)

    llm._transport_call = _transport_call
    llm._atransport_call = _atransport_call
    return True


def patch_ambient_plugins(local_conversation) -> bool:
    """Makes a conversation's ambient plugin discovery find nothing (#187).

    A PATCH OF SDK 1.56 INTERNALS: LocalConversation._ensure_plugins_loaded calls the module-level
    load_available_plugins(work_dir, include_user=True, include_project=True) with no request field,
    setting or environment variable to turn it off. What it finds under the clone's .agents/plugins
    and .openhands/plugins, or the user's home, runs in the run: hooks.json shell commands and
    .mcp.json servers, outside the bridge, from a branch the agent or a contributor wrote. The harness
    attaches no plugin itself, so the ambient set is replaced by an empty one. Skills and the
    repository's AGENTS.md are separate paths and untouched.

    tests/test_no_ambient_plugins.py fails if the SDK renames the symbol or stops calling it."""
    if not callable(getattr(local_conversation, "load_available_plugins", None)):
        print("agent-harness: #187 plugin patch NOT applied: %s has no load_available_plugins (an SDK "
              "upgrade?); a repository's .agents/plugins will run in the conversation" % local_conversation.__name__,
              file=sys.stderr, flush=True)
        return False
    local_conversation.load_available_plugins = lambda *args, **kwargs: {}
    return True


def _install() -> None:
    # agent-server's children, the agent's own commands, must not load this.
    rest = [p for p in os.environ.get("PYTHONPATH", "").split(os.pathsep) if p and os.path.abspath(p) != HERE]
    if rest:
        os.environ["PYTHONPATH"] = os.pathsep.join(rest)
    else:
        os.environ.pop("PYTHONPATH", None)
    try:
        import httpx
        from mcp.client.streamable_http import StreamableHTTPTransport
        from mcp.types import JSONRPCError, JSONRPCResponse
    except ImportError as exc:
        print("agent-harness: F29 patch NOT applied: %s" % exc, file=sys.stderr, flush=True)
    else:
        patch(StreamableHTTPTransport, httpx, (JSONRPCResponse, JSONRPCError))
    try:
        import mcp.shared.session
        from mcp.client.streamable_http import StreamableHTTPTransport
        from opentelemetry import trace
    except ImportError as exc:
        print("agent-harness: #2283 traceparent NOT applied: %s" % exc, file=sys.stderr, flush=True)
    else:
        patch_traceparent(mcp.shared.session, StreamableHTTPTransport, trace)
    try:
        import openai
        from openhands.sdk.llm.exceptions import LLMTimeoutError
        from openhands.sdk.llm.llm import LLM
    except ImportError as exc:
        print("agent-harness: #99 retry and prompt cache NOT applied: %s" % exc, file=sys.stderr, flush=True)
    else:
        patch_cache_breakpoints(LLM)
        patch_model_retry(LLM, openai)
        patch_timeout_retry(LLM, LLMTimeoutError)
    # Fail closed: unlike the patches above, which keep the run available, this one is a security
    # control, so an agent-server that cannot apply it must not start.
    try:
        from openhands.sdk.conversation.impl import local_conversation
    except ImportError as exc:
        raise SystemExit("agent-harness: #187 plugin patch NOT applied: %s" % exc)
    if not patch_ambient_plugins(local_conversation):
        raise SystemExit("agent-harness: #187 plugin patch NOT applied: refusing to start agent-server")


if __name__ == "sitecustomize":
    _install()
