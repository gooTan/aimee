/* test_cli_agy.c: Antigravity CLI adapter — registration, argv policy, and
 * stream-json parsing against lines captured from a live `agy` 1.1.13 run. */
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "provider_cli_adapter.h"

/* Minimal stub needed for provider_cli_adapter_get without the full agent stack */
int agent_execute_with_tools(const agent_t *a, const agent_network_t *n, const char *sys,
                             const char *usr, int max, double temp, agent_result_t *out)
{
   (void)a;
   (void)n;
   (void)sys;
   (void)usr;
   (void)max;
   (void)temp;
   if (out)
   {
      memset(out, 0, sizeof(*out));
      out->success = 1;
   }
   return 0;
}

static void test_agy_adapter_registered(void)
{
   const provider_cli_adapter_t *agy = provider_cli_adapter_get("agy");
   assert(agy != NULL);
   assert(strcmp(agy->cli_kind, "agy") == 0);
   assert(agy->execute == NULL); /* shared loop drives spawn/parse_line */
   assert(agy->spawn != NULL && agy->parse_line != NULL && agy->build_argv != NULL);
   assert(agy->caps.supports_tool_use == 1);
   printf("PASS: agy adapter registered for the shared provider-CLI loop\n");
}

/* The containment contract, asserted as data: headless print mode, stream-json,
 * slash commands disabled, terminal sandbox on, headless permissions approved,
 * and model pinned from config. */
static void test_agy_argv_policy(void)
{
   const provider_cli_adapter_t *agy = provider_cli_adapter_get("agy");
   agent_t agent;
   memset(&agent, 0, sizeof(agent));
   snprintf(agent.model, sizeof(agent.model), "gemini-3.7-flash-low");
   provider_cli_cfg_t cfg = {.agent = &agent};

   char *tokens[64] = {0};
   int split = 0;
   int argc = agy->build_argv(&cfg, tokens, 48, &split);
   assert(argc > 0);

   int saw_print = 0, saw_stream = 0, saw_noslash = 0, saw_plan = 0, saw_sandbox = 0;
   int saw_permissions = 0, saw_model = 0;
   for (int i = 0; i < argc; i++)
   {
      if (strcmp(tokens[i], "-p") == 0)
         saw_print = 1;
      if (strcmp(tokens[i], "stream-json") == 0)
         saw_stream = 1;
      if (strcmp(tokens[i], "--disable-slash-commands") == 0)
         saw_noslash = 1;
      if (strcmp(tokens[i], "--mode") == 0 && i + 1 < argc && strcmp(tokens[i + 1], "plan") == 0)
         saw_plan = 1;
      if (strcmp(tokens[i], "--sandbox") == 0)
         saw_sandbox = 1;
      if (strcmp(tokens[i], "--dangerously-skip-permissions") == 0)
         saw_permissions = 1;
      if (strcmp(tokens[i], "--model") == 0 && i + 1 < argc &&
          strcmp(tokens[i + 1], "gemini-3.7-flash-low") == 0)
         saw_model = 1;
   }
   assert(saw_print && saw_stream && saw_noslash && saw_plan && saw_sandbox && saw_permissions &&
          saw_model);
   provider_cli_free_tokens(tokens, split);
   printf("PASS: agy argv pins the model and enables headless tools inside its sandbox\n");
}

static void test_agy_spawn_refuses_empty_and_oversized_prompts(void)
{
   const provider_cli_adapter_t *agy = provider_cli_adapter_get("agy");
   agent_t agent;
   memset(&agent, 0, sizeof(agent));
   provider_cli_cfg_t cfg = {.agent = &agent};
   int in_fd = -1, out_fd = -1;
   pid_t pid = 0;

   assert(agy->spawn(&cfg, "", &in_fd, &out_fd, &pid) == -1);
   assert(agy->spawn(&cfg, NULL, &in_fd, &out_fd, &pid) == -1);

   /* The prompt travels in argv; refuse before execvp can fail with E2BIG. */
   size_t huge_len = (size_t)(101 * 1024);
   char *huge = malloc(huge_len + 1);
   assert(huge != NULL);
   memset(huge, 'a', huge_len);
   huge[huge_len] = '\0';
   assert(agy->spawn(&cfg, huge, &in_fd, &out_fd, &pid) == -1);
   free(huge);
   assert(pid == 0); /* nothing was spawned */
   printf("PASS: agy spawn refuses empty and oversized argv prompts\n");
}

static void test_agy_parse_text_delta(void)
{
   const provider_cli_adapter_t *agy = provider_cli_adapter_get("agy");
   cli_event_t ev;
   int m = agy->parse_line("{\"event\":\"step_update\",\"step_update\":{\"conversation_id\":\"c1\","
                           "\"step_index\":2,\"state\":\"ACTIVE\",\"step_type\":\"agent_response\","
                           "\"text_delta\":\"pong\"}}",
                           &ev);
   assert(m == 1);
   assert(ev.type == CLI_EVENT_TEXT_DELTA);
   assert(strcmp(ev.text, "pong") == 0);
   printf("PASS: agent_response text_delta becomes TEXT_DELTA\n");
}

static void test_agy_parse_step_usage(void)
{
   const provider_cli_adapter_t *agy = provider_cli_adapter_get("agy");
   cli_event_t ev;
   int m = agy->parse_line(
       "{\"event\":\"step_update\",\"step_update\":{\"step_index\":3,\"state\":\"DONE\","
       "\"step_type\":\"checkpoint\",\"duration_seconds\":0.86,"
       "\"usage\":{\"input_tokens\":99,\"output_tokens\":3,\"thinking_tokens\":0,"
       "\"cache_read_tokens\":0,\"total_tokens\":102}}}",
       &ev);
   assert(m == 1);
   assert(ev.prompt_tokens == 99);
   assert(ev.completion_tokens == 3);
   printf("PASS: DONE step usage latches token counts\n");
}

static void test_agy_parse_result_success(void)
{
   const provider_cli_adapter_t *agy = provider_cli_adapter_get("agy");
   cli_event_t ev;
   int m = agy->parse_line(
       "{\"event\":\"result\",\"result\":{\"conversation_id\":\"c1\",\"status\":\"SUCCESS\","
       "\"response\":\"pong\\n\",\"duration_seconds\":3.17,\"num_turns\":1,"
       "\"usage\":{\"input_tokens\":14921,\"output_tokens\":4,\"thinking_tokens\":33,"
       "\"cache_read_tokens\":7,\"total_tokens\":14925}}}",
       &ev);
   assert(m == 1);
   assert(ev.type == CLI_EVENT_TURN_COMPLETE);
   assert(strcmp(ev.text, "pong\n") == 0);
   assert(ev.prompt_tokens == 14921);
   assert(ev.completion_tokens == 4 + 33); /* thinking tokens are billed output */
   assert(ev.cache_read_tokens == 7);
   assert(ev.latency_ms == 3170);
   printf("PASS: SUCCESS result becomes TURN_COMPLETE with usage and latency\n");
}

static void test_agy_parse_result_failure(void)
{
   const provider_cli_adapter_t *agy = provider_cli_adapter_get("agy");
   cli_event_t ev;
   int m = agy->parse_line(
       "{\"event\":\"result\",\"result\":{\"status\":\"FAILED\",\"error\":\"quota exhausted\"}}",
       &ev);
   assert(m == 1);
   assert(ev.type == CLI_EVENT_ERROR);
   assert(strstr(ev.text, "FAILED") != NULL);
   assert(strstr(ev.text, "quota exhausted") != NULL);
   printf("PASS: non-SUCCESS result becomes an ERROR event\n");
}

static void test_agy_parse_error_event(void)
{
   const provider_cli_adapter_t *agy = provider_cli_adapter_get("agy");
   cli_event_t ev;
   int m = agy->parse_line("{\"event\":\"error\",\"message\":\"sign in required\"}", &ev);
   assert(m == 1);
   assert(ev.type == CLI_EVENT_ERROR);
   assert(strstr(ev.text, "sign in required") != NULL);
   printf("PASS: error event surfaces the message\n");
}

static void test_agy_parse_init_and_noise(void)
{
   const provider_cli_adapter_t *agy = provider_cli_adapter_get("agy");
   cli_event_t ev;
   /* init is matched (kept out of the raw fallback) but carries no event. */
   int m =
       agy->parse_line("{\"event\":\"init\",\"conversation_id\":\"c1\",\"init\":{\"model\":\"g\","
                       "\"tools\":[\"invoke_subagent\"],\"permission_mode\":\"request-review\"}}",
                       &ev);
   assert(m == 1);
   assert(ev.type == CLI_EVENT_NONE);
   assert(agy->parse_line("not json", &ev) == 0);
   assert(agy->parse_line("{\"other\":\"shape\"}", &ev) == 0);
   assert(agy->parse_line(NULL, &ev) == 0);
   printf("PASS: init matched silently; noise unmatched\n");
}

int main(void)
{
   test_agy_adapter_registered();
   test_agy_argv_policy();
   test_agy_spawn_refuses_empty_and_oversized_prompts();
   test_agy_parse_text_delta();
   test_agy_parse_step_usage();
   test_agy_parse_result_success();
   test_agy_parse_result_failure();
   test_agy_parse_error_event();
   test_agy_parse_init_and_noise();
   printf("ALL PASS\n");
   return 0;
}
