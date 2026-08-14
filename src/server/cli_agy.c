/* cli_agy.c: Antigravity CLI (`agy`) provider-CLI adapter.
 *
 * Drives `agy -p <prompt> --output-format stream-json` and parses its
 * newline-delimited event stream:
 *
 *   {"event":"init","conversation_id":"...","init":{"model":"...","tools":[...]}}
 *   {"event":"step_update","step_update":{"step_type":"agent_response",
 *       "text_delta":"...","state":"ACTIVE|DONE","usage":{...}}}
 *   {"event":"result","result":{"status":"SUCCESS","response":"...",
 *       "duration_seconds":n,"usage":{"input_tokens":n,"output_tokens":n,
 *       "thinking_tokens":n,"cache_read_tokens":n}}}
 *
 * Containment: aimee never passes --dangerously-skip-permissions. In headless
 * print mode agy auto-denies every tool that needs a permission grant, which
 * includes its subagent tools (define_subagent, invoke_subagent,
 * browser_subagent) and mutating shell/file tools — so a delegate cannot spawn
 * nested agents or write outside what the operator granted in agy's own
 * settings. --disable-slash-commands additionally stops prompt-driven skill
 * expansion from smuggling instructions into the turn.
 *
 * The prompt travels in argv because agy's print mode does not read it from
 * stdin. That imposes the platform's per-argument size limit, so spawn refuses
 * oversized prompts with a clear diagnostic instead of letting execvp fail
 * with E2BIG. Keep agy seats on bounded-prompt roles (review, explain).
 *
 * Conversation resume: every event carries a conversation_id and the CLI
 * accepts --conversation <id>; an operator can resume a turn by appending that
 * flag to the agent's cli_cmd. The one-shot delegate contract itself is
 * stateless, so the adapter does not persist ids.
 */
#include "provider_cli_adapter.h"

#include <stddef.h>

#include "cJSON.h"

#include <stdio.h>
#include <string.h>

#define AGY_ARG_MAX 48
/* Stay under the kernel's usual 128 KiB single-argument ceiling with margin
 * for environment overhead. */
#define AGY_PROMPT_ARGV_MAX (100 * 1024)

static int agy_build_argv_with_prompt(const provider_cli_cfg_t *cfg, const char *task_prompt,
                                      char **tokens, int cap, int *split_count)
{
   char err[128];
   const agent_t *agent = cfg ? cfg->agent : NULL;
   const char *cmd = (agent && agent->cli_cmd[0]) ? agent->cli_cmd : "agy";
   int count = provider_cli_split_command(cmd, tokens, cap, err, sizeof(err));
   if (count < 0)
      return -1;
   if (split_count)
      *split_count = count;

   int argc = count;

#define AGY_ADD_ARG(s)                                                                             \
   do                                                                                              \
   {                                                                                               \
      if (argc >= cap)                                                                             \
      {                                                                                            \
         provider_cli_free_tokens(tokens, count);                                                  \
         return -1;                                                                                \
      }                                                                                            \
      tokens[argc++] = (char *)(s);                                                                \
      tokens[argc] = NULL;                                                                         \
   } while (0)

   AGY_ADD_ARG("-p");
   if (task_prompt && task_prompt[0])
      AGY_ADD_ARG(task_prompt);
   AGY_ADD_ARG("--output-format");
   AGY_ADD_ARG("stream-json");
   AGY_ADD_ARG("--disable-slash-commands");
   if (agent && agent->model[0])
   {
      AGY_ADD_ARG("--model");
      AGY_ADD_ARG(agent->model);
   }
#undef AGY_ADD_ARG
   return argc;
}

/* Remote/thin-client argv builder: same shape, no prompt token (the detached
 * exec path supplies the prompt itself). */
static int agy_build_argv(const provider_cli_cfg_t *cfg, char **tokens, int cap, int *split_count)
{
   return agy_build_argv_with_prompt(cfg, NULL, tokens, cap, split_count);
}

static int agy_spawn(const provider_cli_cfg_t *cfg, const char *task_prompt, int *stdin_fd,
                     int *stdout_fd, pid_t *pid_out)
{
   if (!task_prompt || !task_prompt[0])
      return -1;
   if (strlen(task_prompt) > AGY_PROMPT_ARGV_MAX)
      return -1;
   char *tokens[AGY_ARG_MAX + 1] = {0};
   int split = 0;
   int argc = agy_build_argv_with_prompt(cfg, task_prompt, tokens, AGY_ARG_MAX, &split);
   if (argc < 0)
      return -1;

   int rc = provider_cli_spawn_argv(cfg, tokens, stdin_fd, stdout_fd, pid_out);
   provider_cli_free_tokens(tokens, split);
   return rc;
}

static int agy_build_prompt(const provider_cli_cfg_t *cfg, const char *system_prompt,
                            const char *task, char *buf, size_t buf_sz)
{
   /* agy has no --append-system-prompt equivalent, so a configured system
    * prompt is prepended to the task text. */
   (void)cfg;
   if (!task || !task[0] || !buf || buf_sz == 0)
      return -1;
   if (system_prompt && system_prompt[0])
      snprintf(buf, buf_sz, "%s\n\n%s", system_prompt, task);
   else
      snprintf(buf, buf_sz, "%s", task);
   return 0;
}

static const char *agy_json_string(cJSON *obj, const char *name)
{
   cJSON *v = obj ? cJSON_GetObjectItem(obj, name) : NULL;
   return (v && cJSON_IsString(v)) ? v->valuestring : NULL;
}

static void agy_parse_usage(cJSON *usage, cli_event_t *event_out)
{
   if (!usage || !cJSON_IsObject(usage))
      return;
   cJSON *input = cJSON_GetObjectItem(usage, "input_tokens");
   if (input && cJSON_IsNumber(input))
      event_out->prompt_tokens = input->valueint;
   cJSON *output = cJSON_GetObjectItem(usage, "output_tokens");
   if (output && cJSON_IsNumber(output))
      event_out->completion_tokens = output->valueint;
   /* Thinking tokens are billed output; fold them into the completion count so
    * routing and cost accounting see the real spend. */
   cJSON *thinking = cJSON_GetObjectItem(usage, "thinking_tokens");
   if (thinking && cJSON_IsNumber(thinking) && thinking->valueint > 0)
      event_out->completion_tokens += thinking->valueint;
   cJSON *cache_read = cJSON_GetObjectItem(usage, "cache_read_tokens");
   if (cache_read && cJSON_IsNumber(cache_read))
      event_out->cache_read_tokens = cache_read->valueint;
}

static int agy_parse_line(const char *line, cli_event_t *event_out)
{
   if (!line || !event_out)
      return 0;
   memset(event_out, 0, sizeof(*event_out));

   cJSON *obj = cJSON_Parse(line);
   if (!obj || !cJSON_IsObject(obj))
   {
      if (obj)
         cJSON_Delete(obj);
      return 0;
   }

   int matched = 0;
   const char *event = agy_json_string(obj, "event");
   if (event && strcmp(event, "step_update") == 0)
   {
      cJSON *step = cJSON_GetObjectItem(obj, "step_update");
      const char *step_type = agy_json_string(step, "step_type");
      const char *delta = agy_json_string(step, "text_delta");
      if (step_type && strcmp(step_type, "agent_response") == 0 && delta && delta[0])
      {
         event_out->type = CLI_EVENT_TEXT_DELTA;
         snprintf(event_out->text, sizeof(event_out->text), "%s", delta);
         matched = 1;
      }
      else if (step_type && strcmp(step_type, "tool_use") == 0)
      {
         const char *tool = agy_json_string(step, "tool_name");
         event_out->type = CLI_EVENT_TOOL_START;
         snprintf(event_out->tool_name, sizeof(event_out->tool_name), "%s",
                  (tool && tool[0]) ? tool : "tool");
         event_out->write_event = provider_cli_event_is_write(event_out);
         matched = 1;
      }
      /* Usage arrives on DONE step updates and on the final result; latch
       * whichever is seen so a stream cut before the result still reports. */
      agy_parse_usage(step ? cJSON_GetObjectItem(step, "usage") : NULL, event_out);
      matched = matched || event_out->prompt_tokens || event_out->completion_tokens ||
                event_out->cache_read_tokens;
   }
   else if (event && strcmp(event, "result") == 0)
   {
      cJSON *result = cJSON_GetObjectItem(obj, "result");
      const char *status = agy_json_string(result, "status");
      const char *response = agy_json_string(result, "response");
      if (status && strcmp(status, "SUCCESS") != 0)
      {
         const char *error = agy_json_string(result, "error");
         event_out->type = CLI_EVENT_ERROR;
         snprintf(event_out->text, sizeof(event_out->text), "agy result status %s%s%s", status,
                  (error && error[0]) ? ": " : "", (error && error[0]) ? error : "");
         matched = 1;
      }
      else
      {
         event_out->type = CLI_EVENT_TURN_COMPLETE;
         if (response && response[0])
            snprintf(event_out->text, sizeof(event_out->text), "%s", response);
         matched = 1;
      }
      agy_parse_usage(result ? cJSON_GetObjectItem(result, "usage") : NULL, event_out);
      cJSON *duration = result ? cJSON_GetObjectItem(result, "duration_seconds") : NULL;
      if (duration && cJSON_IsNumber(duration))
         event_out->latency_ms = (int)(duration->valuedouble * 1000.0);
   }
   else if (event && strcmp(event, "error") == 0)
   {
      const char *message = agy_json_string(obj, "message");
      if (!message)
         message = agy_json_string(cJSON_GetObjectItem(obj, "error"), "message");
      event_out->type = CLI_EVENT_ERROR;
      snprintf(event_out->text, sizeof(event_out->text), "agy error: %s",
               (message && message[0]) ? message : line);
      matched = 1;
   }
   else if (event && strcmp(event, "init") == 0)
   {
      /* Init is protocol noise for the one-shot contract, but marking it
       * matched keeps it out of the raw-output fallback response. */
      matched = 1;
   }

   cJSON_Delete(obj);
   return matched;
}

const provider_cli_adapter_t agy_provider_cli_adapter = {
    .cli_kind = "agy",
    .display_name = "Antigravity CLI",
    .caps = {.max_context_tokens = 200000,
             .supports_tool_use = 1,
             .proto_stability = PROVIDER_CLI_PROTO_STABLE,
             .write_confidence = 0.85f},
    .spawn = agy_spawn,
    .parse_line = agy_parse_line,
    .format_tool_result = provider_cli_format_json_tool_result,
    .is_write_event = provider_cli_event_is_write,
    .build_prompt = agy_build_prompt,
    .build_argv = agy_build_argv,
    .execute = NULL,
};
