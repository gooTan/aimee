/* test_cli_oracle.c: Oracle consultation adapter — registration and argv
 * policy. The adapter is execute-style (temp task file in, --write-output
 * answer file out), so the pure surface under test is oracle_build_argv. */
#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "cli_oracle.h"
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

static void test_oracle_adapter_registered(void)
{
   const provider_cli_adapter_t *oracle = provider_cli_adapter_get("oracle");
   assert(oracle != NULL);
   assert(strcmp(oracle->cli_kind, "oracle") == 0);
   assert(oracle->execute != NULL); /* execute-style: file in, file out */
   assert(oracle->spawn == NULL && oracle->parse_line == NULL);
   /* Consultation only: no tools, and zero write confidence so routing can
    * never treat this seat as an implementer. */
   assert(oracle->caps.supports_tool_use == 0);
   assert(oracle->caps.write_confidence == 0.0f);
   printf("PASS: oracle adapter registered as a read-only consultation seat\n");
}

static int argv_has_pair(char **tokens, int argc, const char *flag, const char *value)
{
   for (int i = 0; i + 1 < argc; i++)
   {
      if (strcmp(tokens[i], flag) == 0 && strcmp(tokens[i + 1], value) == 0)
         return 1;
   }
   return 0;
}

static void test_oracle_argv_policy(void)
{
   agent_t agent;
   memset(&agent, 0, sizeof(agent));
   snprintf(agent.cli_cmd, sizeof(agent.cli_cmd), "oracle -e browser");
   snprintf(agent.model, sizeof(agent.model), "gpt-5.5-pro");
   provider_cli_cfg_t cfg = {.agent = &agent};

   char *tokens[64] = {0};
   int split = 0;
   int argc = oracle_build_argv(&cfg, "/tmp/task.md", "/tmp/answer.md", 2700, tokens, 48, &split);
   assert(argc > 0);
   /* The operator's engine choice from cli_cmd survives verbatim. */
   assert(strcmp(tokens[0], "oracle") == 0);
   assert(strcmp(tokens[1], "-e") == 0 && strcmp(tokens[2], "browser") == 0);
   /* Fixed consultation shape: task attached as a file, answer to a file,
    * no desktop notifications, aligned timeout, pinned model. */
   assert(argv_has_pair(tokens, argc, "-f", "/tmp/task.md"));
   assert(argv_has_pair(tokens, argc, "--write-output", "/tmp/answer.md"));
   assert(argv_has_pair(tokens, argc, "--timeout", "2700"));
   assert(argv_has_pair(tokens, argc, "-m", "gpt-5.5-pro"));
   int saw_notify_off = 0, saw_prompt = 0;
   for (int i = 0; i < argc; i++)
   {
      if (strcmp(tokens[i], "--no-notify") == 0)
         saw_notify_off = 1;
      if (strcmp(tokens[i], "-p") == 0)
         saw_prompt = 1;
      /* The real prompt must never travel in argv: only the pointer text. */
      assert(strstr(tokens[i], "frozen_diff") == NULL);
   }
   assert(saw_notify_off && saw_prompt);
   provider_cli_free_tokens(tokens, split);
   printf("PASS: oracle argv attaches the task file and pins model/timeout\n");
}

static void test_oracle_argv_without_model(void)
{
   agent_t agent;
   memset(&agent, 0, sizeof(agent));
   provider_cli_cfg_t cfg = {.agent = &agent};
   char *tokens[64] = {0};
   int split = 0;
   int argc = oracle_build_argv(&cfg, "/tmp/t.md", "/tmp/a.md", 60, tokens, 48, &split);
   assert(argc > 0);
   for (int i = 0; i < argc; i++)
      assert(strcmp(tokens[i], "-m") != 0);  /* unpinned agent adds no -m */
   assert(strcmp(tokens[0], "oracle") == 0); /* default command */
   provider_cli_free_tokens(tokens, split);
   printf("PASS: unpinned oracle agent omits the model flag\n");
}

int main(void)
{
   test_oracle_adapter_registered();
   test_oracle_argv_policy();
   test_oracle_argv_without_model();
   printf("ALL PASS\n");
   return 0;
}
