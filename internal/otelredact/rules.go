package otelredact

import "regexp"

// The rules deploy/otel/collector.yaml applies, as data: one list per decision, so the
// collector and Livy's own receiver remove the same things. rules_test.go reads
// collector.yaml and fails when the two drift apart.

const claudeService = "claude-code"

var (
	// Identity: who the person is and which machine they used, on every attribute set.
	identityPrefix = regexp.MustCompile(`^(user|enduser|host|process)\.`)
	identityKeys   = keySet("organization.id", "identity.source", "workspace.host_paths", "cwd")

	// Claude Code's log content, sent only when a content flag is on; error is free text.
	logContent = keySet("prompt", "prompt_text", "response", "tool_parameters", "tool_input", "body",
		"body_ref", "system_prompt", "error")

	// Claude Code's span content attributes, and free error text on any span.
	spanContent = keySet("user_prompt", "new_context", "system_reminders", "system_prompt_preview",
		"user_system_prompt", "response.model_output", "tool_input", "file_path", "full_command",
		"hook_definitions", "error")

	// The only Claude Code span event kept: tool output and exception events carry text.
	claudeEventKeep = "gen_ai.request.attempt"

	// A span event name that is not an identifier may be a formatted message: replaced.
	eventName = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

	// Every client but Claude Code (Codex, today) keeps only these attributes.
	foreignLogKeep = keySet(
		"event.name", "event.kind", "event.timestamp", "conversation.id", "app.version", "auth_mode", "originator",
		"terminal.type", "model", "slug", "duration_ms", "http.response.status_code", "attempt", "endpoint",
		"success", "input_token_count", "output_token_count", "cached_token_count", "cache_write_token_count",
		"reasoning_token_count", "tool_token_count", "ttft_ms", "service_tier", "model_reasoning_effort",
		"tool_name", "tool_namespace", "call_id", "tool_result_seq", "output_truncated", "decision", "source",
		"prompt_length", "provider_name", "reasoning_effort", "reasoning_summary", "context_window",
		"auto_compact_token_limit", "approval_policy", "sandbox_policy", "startup.phase", "startup.status",
		"turn.id", "usage.estimated_usd", "turn.interrupted", "speed",
	)
	foreignSpanKeep = keySet(
		"aborted", "allow_provider_model_fallback", "api.path", "app_server.api_version", "app_server.client_name",
		"app_server.client_version", "app_server.connection_id", "apps_enabled", "build_mode", "busy_ns",
		"call_id", "catalog_entries", "catalog_entry_count", "cell.id", "code.line.number", "code.module.name",
		"codex.op", "codex.request.reasoning_effort", "codex.response.receive_wait_ms", "codex.tool.phase",
		"codex.turn.phase", "codex.turn.reasoning_effort", "codex.turn.token_usage.cache_write_input_tokens",
		"codex.turn.token_usage.cached_input_tokens", "codex.turn.token_usage.input_tokens",
		"codex.turn.token_usage.non_cached_input_tokens", "codex.turn.token_usage.output_tokens",
		"codex.turn.token_usage.reasoning_output_tokens", "codex.turn.token_usage.total_tokens",
		"codex.usage.reasoning_output_tokens", "codex.usage.total_tokens", "configuration_pending",
		"connector_count", "conversation.id", "dynamic_tool_count", "environment_count", "environment_id",
		"exit_signaled", "gen_ai.usage.cache_read.input_tokens", "gen_ai.usage.cache_write.input_tokens",
		"gen_ai.usage.input_tokens", "gen_ai.usage.output_tokens", "http.method", "http.request.method",
		"http.response.status_code", "idle_ns", "input_count", "item_count", "max_total", "mcp.method", "mode",
		"model", "model.provided", "non_blocking", "outcome", "output_closed", "phase", "plugins_enabled",
		"provider", "refresh_strategy", "remote", "remote_plugin_enabled", "root_count", "rpc.method",
		"rpc.request_id", "rpc.system", "rpc.transport", "runtime_tool_call_id", "sandboxed", "search_info_count",
		"selected_skill_count", "server.address", "server.port", "server_name", "session_init.ephemeral",
		"session_init.required_mcp_server_count", "stop_reason", "submission.id", "target", "thread.id",
		"thread.name", "thread_id", "thread_start.dynamic_tool_count", "thread_start.experimental_raw_events",
		"tool_name", "tool_source", "tool_spec_count", "transport", "turn.has_metadata_header", "turn.id",
		"turn_id", "unified_exec_process_id", "user_input_count", "websocket.warmup", "wire_api",
	)
	foreignEventKeep = keySet(
		"event.name", "event.kind", "event.timestamp", "level", "target", "conversation.id", "app.version",
		"auth_mode", "originator", "terminal.type", "model", "slug", "duration_ms", "http.response.status_code",
		"attempt", "endpoint", "success", "input_token_count", "output_token_count", "cached_token_count",
		"cache_write_token_count", "reasoning_token_count", "tool_token_count", "ttft_ms", "service_tier",
		"model_reasoning_effort", "tool_name", "tool_namespace", "call_id", "tool_result_seq", "output_truncated",
		"decision", "source", "prompt_length", "provider_name", "reasoning_effort", "reasoning_summary",
		"context_window", "auto_compact_token_limit", "approval_policy", "sandbox_policy", "startup.phase",
		"startup.status", "turn.id", "usage.estimated_usd", "turn.interrupted", "speed",
	)
	foreignPointKeep = keySet(
		"active", "actual_store", "after_prewarm", "app.version", "auth_mode", "backend", "bundle_shape", "cache",
		"candidate_set_truncated", "catalog_surface", "checker", "command_category", "config_use_memories",
		"credential_kind", "current_item_type", "db", "dependency", "error_kind", "event", "exposure",
		"fallback_reason", "feature_enabled", "format", "has_citations", "has_deny_globs", "incremental_tools",
		"is_git", "is_worktree", "kind", "method", "mode", "model", "multiple_hard_links", "multiple_write_roots",
		"operation", "originator", "outcome", "phase", "previous_item_type", "product_sku", "query_truncated",
		"read_allowed", "read_progress", "reason", "result", "sandbox", "sandbox_policy", "secure_outcome",
		"selected_store", "server_kind", "session_source", "shell_snapshot_mode", "source", "stage", "state",
		"status", "storage_phase", "store_mode", "success", "target_resolution_changed", "tmp_mem_enabled",
		"token_type", "tool", "tool_mode", "trigger", "tty", "version",
	)
)

func keySet(keys ...string) map[string]bool {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	return m
}
