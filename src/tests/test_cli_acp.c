#include <assert.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <unistd.h>

#include "cli_acp.h"
#include "cJSON.h"
#include "provider_cli_adapter.h"
#include "platform_test_util.h" /* platform_tmpdir: honour TMPDIR, do not leak into /tmp */

/* Minimal stubs needed for provider_cli_adapter_get without the full agent stack */
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

static void test_acp_adapter_registered(void)
{
   const provider_cli_adapter_t *acp = provider_cli_adapter_get("acp");
   assert(acp != NULL);
   assert(strcmp(acp->cli_kind, "acp") == 0);
   assert(acp->execute != NULL);
   assert(acp->spawn == NULL);
   printf("PASS: acp adapter registered with cli_kind='acp' and execute set\n");
}

static void test_acp_not_found_for_unknown(void)
{
   const provider_cli_adapter_t *x = provider_cli_adapter_get("not-a-real-kind");
   assert(x == NULL);
   printf("PASS: unknown cli_kind returns NULL\n");
}

static void test_acp_caps(void)
{
   const provider_cli_adapter_t *acp = provider_cli_adapter_get("acp");
   assert(acp != NULL);
   assert(acp->caps.supports_tool_use == 1);
   assert(acp->caps.write_confidence > 0.0f);
   printf("PASS: acp adapter caps look sane\n");
}

/* ---- outbound turn parser (acp_turn_consume) ---- */

static void test_turn_text_delta(void)
{
   /* Legacy aimee dialect: text/delta notifications accumulate. */
   acp_turn_state_t st = {0};
   assert(acp_turn_consume("{\"jsonrpc\":\"2.0\",\"method\":\"text/delta\","
                           "\"params\":{\"content\":\"po\"}}",
                           &st) == 0);
   assert(acp_turn_consume("{\"jsonrpc\":\"2.0\",\"method\":\"text/delta\","
                           "\"params\":{\"content\":\"ng\"}}",
                           &st) == 0);
   assert(st.text && strcmp(st.text, "pong") == 0);
   assert(!st.done);
   free(st.text);
   printf("PASS: text/delta deltas accumulate\n");
}

static void test_turn_session_update_string(void)
{
   /* aimee's own inbound server streams session/update with a string content
    * and a type of agent_message_chunk. These must accumulate too — the bug
    * this fix addresses (previously session/update was ignored). */
   acp_turn_state_t st = {0};
   assert(acp_turn_consume("{\"jsonrpc\":\"2.0\",\"method\":\"session/update\","
                           "\"params\":{\"sessionId\":\"s1\",\"update\":"
                           "{\"type\":\"agent_message_chunk\",\"content\":\"he\"}}}",
                           &st) == 0);
   assert(acp_turn_consume("{\"jsonrpc\":\"2.0\",\"method\":\"session/update\","
                           "\"params\":{\"sessionId\":\"s1\",\"update\":"
                           "{\"type\":\"agent_message_chunk\",\"content\":\"llo\"}}}",
                           &st) == 0);
   assert(st.text && strcmp(st.text, "hello") == 0);
   free(st.text);
   printf("PASS: session/update string content accumulates\n");
}

static void test_turn_session_update_typed_object(void)
{
   /* Real ACP (Zed) carries content as a typed {type:"text",text:...} object. */
   acp_turn_state_t st = {0};
   assert(acp_turn_consume("{\"jsonrpc\":\"2.0\",\"method\":\"session/update\","
                           "\"params\":{\"update\":{\"sessionUpdate\":\"agent_message_chunk\","
                           "\"content\":{\"type\":\"text\",\"text\":\"hi there\"}}}}",
                           &st) == 0);
   assert(st.text && strcmp(st.text, "hi there") == 0);
   free(st.text);
   printf("PASS: session/update typed object content accumulates\n");
}

static void test_turn_tool_call_counts(void)
{
   acp_turn_state_t st = {0};
   /* legacy tool/call */
   assert(acp_turn_consume("{\"jsonrpc\":\"2.0\",\"method\":\"tool/call\","
                           "\"params\":{\"name\":\"grep\"}}",
                           &st) == 0);
   /* ACP session/update tool_call */
   assert(acp_turn_consume("{\"jsonrpc\":\"2.0\",\"method\":\"session/update\","
                           "\"params\":{\"update\":{\"sessionUpdate\":\"tool_call\","
                           "\"title\":\"read_file\"}}}",
                           &st) == 0);
   assert(st.tool_calls == 2);
   assert(st.text == NULL); /* tool calls contribute no reply text */
   printf("PASS: tool calls counted from both dialects\n");
}

static void test_turn_result_fallback(void)
{
   /* With no streamed text, the terminal result.content becomes the reply. */
   acp_turn_state_t st = {0};
   assert(acp_turn_consume("{\"jsonrpc\":\"2.0\",\"id\":2,"
                           "\"result\":{\"content\":\"final answer\"}}",
                           &st) == 0);
   assert(st.done);
   assert(st.text && strcmp(st.text, "final answer") == 0);
   free(st.text);
   printf("PASS: result.content used as fallback reply\n");
}

static void test_turn_stream_beats_result(void)
{
   /* Streamed text takes precedence; result.content is ignored when text exists. */
   acp_turn_state_t st = {0};
   assert(acp_turn_consume("{\"jsonrpc\":\"2.0\",\"method\":\"session/update\","
                           "\"params\":{\"update\":{\"type\":\"agent_message_chunk\","
                           "\"content\":\"streamed\"}}}",
                           &st) == 0);
   assert(acp_turn_consume("{\"jsonrpc\":\"2.0\",\"id\":2,"
                           "\"result\":{\"content\":\"DUPLICATE\"}}",
                           &st) == 0);
   assert(st.done);
   assert(st.text && strcmp(st.text, "streamed") == 0);
   free(st.text);
   printf("PASS: streamed text wins over result.content\n");
}

static void test_turn_standard_acp_terminal(void)
{
   /* Standard ACP: text streams via session/update; the turn ends with the
    * session/prompt response (id == prompt_id) carrying {stopReason}, no
    * content. The id-2 default must NOT end the turn in this mode. */
   acp_turn_state_t st = {.prompt_id = 3};
   assert(acp_turn_consume("{\"jsonrpc\":\"2.0\",\"method\":\"session/update\","
                           "\"params\":{\"update\":{\"sessionUpdate\":\"agent_message_chunk\","
                           "\"content\":{\"type\":\"text\",\"text\":\"hi\"}}}}",
                           &st) == 0);
   /* an id-2 response (e.g. a stray) does not terminate a prompt_id=3 turn */
   assert(acp_turn_consume("{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"sessionId\":\"s\"}}",
                           &st) == 0);
   assert(!st.done);
   /* the session/prompt response (id 3, stopReason) ends the turn */
   assert(acp_turn_consume(
              "{\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{\"stopReason\":\"end_turn\"}}", &st) == 0);
   assert(st.done);
   assert(st.text && strcmp(st.text, "hi") == 0); /* reply came from the stream */
   free(st.text);
   printf("PASS: standard ACP terminal is the session/prompt response\n");
}

static void test_turn_error(void)
{
   acp_turn_state_t st = {0};
   int rc = acp_turn_consume("{\"jsonrpc\":\"2.0\",\"id\":2,"
                             "\"error\":{\"code\":-32000,\"message\":\"boom\"}}",
                             &st);
   assert(rc == -1);
   assert(st.had_error);
   assert(strstr(st.error, "boom") != NULL);
   free(st.text);
   printf("PASS: error response returns -1 with message\n");
}

static void test_turn_ignores_noise(void)
{
   acp_turn_state_t st = {0};
   assert(acp_turn_consume("not json", &st) == 0);
   assert(acp_turn_consume("", &st) == 0);
   assert(acp_turn_consume(NULL, &st) == 0);
   /* initialize ack (id=1) is neither a delta nor the terminal result. */
   assert(acp_turn_consume("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}", &st) == 0);
   assert(st.text == NULL && !st.done && !st.had_error);
   printf("PASS: noise / non-terminal responses ignored\n");
}

/* ---- client-side tool serving (acp_serve_client_request) ---- */

static char *make_workdir(char *dir, size_t sz)
{
   snprintf(dir, sz, "%s/acp_serve_XXXXXX", platform_tmpdir());
   char *d = mkdtemp(dir);
   assert(d != NULL);
   return d;
}

static void test_serve_write_then_read(void)
{
   char wdbuf[64];
   char *wd = make_workdir(wdbuf, sizeof(wdbuf));
   char *resp = NULL;

   /* Agent writes a new file (in a not-yet-existing subdir). */
   int r = acp_serve_client_request(
       "{\"jsonrpc\":\"2.0\",\"id\":10,\"method\":\"fs/write_text_file\","
       "\"params\":{\"path\":\"sub/out.txt\",\"content\":\"hello world\"}}",
       wd, &resp);
   assert(r == 1 && resp != NULL);
   assert(strstr(resp, "\"error\"") == NULL); /* success: result, not error */
   free(resp);
   char full[512];
   snprintf(full, sizeof(full), "%s/sub/out.txt", wd);
   FILE *f = fopen(full, "rb");
   assert(f != NULL);
   char buf[64] = {0};
   size_t n = fread(buf, 1, sizeof(buf) - 1, f);
   fclose(f);
   buf[n] = '\0';
   assert(strcmp(buf, "hello world") == 0);

   /* Agent reads it back; the response carries the content. */
   resp = NULL;
   r = acp_serve_client_request("{\"jsonrpc\":\"2.0\",\"id\":11,\"method\":\"fs/read_text_file\","
                                "\"params\":{\"path\":\"sub/out.txt\"}}",
                                wd, &resp);
   assert(r == 1 && resp != NULL);
   assert(strstr(resp, "hello world") != NULL);
   assert(strstr(resp, "\"error\"") == NULL);
   free(resp);

   unlink(full);
   printf("PASS: fs write then read round-trips within workdir\n");
}

static void test_serve_path_sandbox(void)
{
   char wdbuf[64];
   char *wd = make_workdir(wdbuf, sizeof(wdbuf));
   char *resp = NULL;
   /* A ".." escape is rejected with an error (but still a handled request). */
   int r =
       acp_serve_client_request("{\"jsonrpc\":\"2.0\",\"id\":12,\"method\":\"fs/read_text_file\","
                                "\"params\":{\"path\":\"../../etc/passwd\"}}",
                                wd, &resp);
   assert(r == 1 && resp != NULL);
   assert(strstr(resp, "\"error\"") != NULL);
   free(resp);

   /* An absolute path outside the workdir is rejected too. */
   resp = NULL;
   r = acp_serve_client_request("{\"jsonrpc\":\"2.0\",\"id\":13,\"method\":\"fs/write_text_file\","
                                "\"params\":{\"path\":\"/etc/aimee_pwn\",\"content\":\"x\"}}",
                                wd, &resp);
   assert(r == 1 && resp != NULL);
   assert(strstr(resp, "\"error\"") != NULL);
   free(resp);
   assert(access("/etc/aimee_pwn", F_OK) != 0); /* nothing written outside */
   printf("PASS: fs path traversal / absolute escape rejected\n");
}

static void test_serve_permission_autoapprove(void)
{
   char *resp = NULL;
   int r = acp_serve_client_request(
       "{\"jsonrpc\":\"2.0\",\"id\":14,\"method\":\"session/request_permission\",\"params\":{"
       "\"options\":[{\"optionId\":\"rej\",\"kind\":\"reject_once\"},"
       "{\"optionId\":\"yes\",\"kind\":\"allow_once\"}]}}",
       "/tmp", &resp);
   assert(r == 1 && resp != NULL);
   assert(strstr(resp, "\"selected\"") != NULL);
   assert(strstr(resp, "\"yes\"") != NULL); /* picked the allow option, not the reject */
   free(resp);
   printf("PASS: permission request auto-approves an allow option\n");
}

static void test_serve_unknown_and_nonrequests(void)
{
   char *resp = NULL;
   /* Unknown agent request (method+id) -> method-not-found so the agent proceeds. */
   int r = acp_serve_client_request(
       "{\"jsonrpc\":\"2.0\",\"id\":15,\"method\":\"terminal/create\",\"params\":{}}", "/tmp",
       &resp);
   assert(r == 1 && resp != NULL && strstr(resp, "-32601") != NULL);
   free(resp);

   /* A notification (method, no id) is not a request -> left for acp_turn_consume. */
   resp = NULL;
   r = acp_serve_client_request("{\"jsonrpc\":\"2.0\",\"method\":\"session/update\","
                                "\"params\":{\"update\":{\"content\":\"hi\"}}}",
                                "/tmp", &resp);
   assert(r == 0 && resp == NULL);

   /* A response (id, no method) is not a request either. */
   r = acp_serve_client_request("{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"content\":\"x\"}}",
                                "/tmp", &resp);
   assert(r == 0 && resp == NULL);
   printf("PASS: unknown request errors; notifications/responses pass through\n");
}

static void test_serve_write_denied_when_read_only(void)
{
   char wdbuf[64];
   char *wd = make_workdir(wdbuf, sizeof(wdbuf));
   char *resp = NULL;
   int r = acp_serve_client_request_gated(
       "{\"jsonrpc\":\"2.0\",\"id\":16,\"method\":\"fs/write_text_file\","
       "\"params\":{\"path\":\"out.txt\",\"content\":\"nope\"}}",
       wd, 0, &resp);
   assert(r == 1 && resp != NULL);
   assert(strstr(resp, "\"error\"") != NULL);
   assert(strstr(resp, "read-only") != NULL);
   free(resp);
   char full[512];
   snprintf(full, sizeof(full), "%s/out.txt", wd);
   assert(access(full, F_OK) != 0); /* nothing was written */

   /* Reads still work for a read-only delegate. */
   resp = NULL;
   snprintf(full, sizeof(full), "%s/in.txt", wd);
   FILE *f = fopen(full, "wb");
   assert(f && fputs("readable", f) >= 0 && fclose(f) == 0);
   r = acp_serve_client_request_gated(
       "{\"jsonrpc\":\"2.0\",\"id\":17,\"method\":\"fs/read_text_file\","
       "\"params\":{\"path\":\"in.txt\"}}",
       wd, 0, &resp);
   assert(r == 1 && resp != NULL);
   assert(strstr(resp, "readable") != NULL && strstr(resp, "\"error\"") == NULL);
   free(resp);
   unlink(full);
   printf("PASS: read-only gate refuses writes but serves reads\n");
}

static void test_serve_permission_denied_when_read_only(void)
{
   char *resp = NULL;
   int r = acp_serve_client_request_gated(
       "{\"jsonrpc\":\"2.0\",\"id\":18,\"method\":\"session/request_permission\",\"params\":{"
       "\"options\":[{\"optionId\":\"yes\",\"kind\":\"allow_once\"},"
       "{\"optionId\":\"rej\",\"kind\":\"reject_once\"}]}}",
       "/tmp", 0, &resp);
   assert(r == 1 && resp != NULL);
   assert(strstr(resp, "\"rej\"") != NULL); /* picked the reject option */
   assert(strstr(resp, "\"yes\"") == NULL);
   free(resp);

   /* With no reject option offered, the read-only gate cancels outright. */
   resp = NULL;
   r = acp_serve_client_request_gated(
       "{\"jsonrpc\":\"2.0\",\"id\":19,\"method\":\"session/request_permission\",\"params\":{"
       "\"options\":[{\"optionId\":\"yes\",\"kind\":\"allow_always\"}]}}",
       "/tmp", 0, &resp);
   assert(r == 1 && resp != NULL);
   assert(strstr(resp, "\"cancelled\"") != NULL);
   assert(strstr(resp, "\"yes\"") == NULL);
   free(resp);
   printf("PASS: read-only gate rejects/cancels permission requests\n");
}

/* ---- reasoning-effort exact-or-fail transport (integration) ---- */

static void do_effort_case(const char *effort, const char *mode, int expect_ok,
                           int expect_has_effort, int expect_has_prompt)
{
   char tpath[256];
   snprintf(tpath, sizeof(tpath), "%s/acp_transcript_XXXXXX", platform_tmpdir());
   int tf = mkstemp(tpath);
   assert(tf >= 0);
   close(tf);

   char fpath[256];
   snprintf(fpath, sizeof(fpath), "%s/aimee-fake-acp-XXXXXX", platform_tmpdir());
   int fd = mkstemp(fpath);
   assert(fd >= 0);
   FILE *f = fdopen(fd, "w");
   assert(f != NULL);
   fprintf(
       f,
       "#!/bin/sh\n"
       "TRANSCRIPT=\"%s\"\n"
       "MODE=\"%s\"\n"
       "while IFS= read -r line; do\n"
       "  printf '%%s\\n' \"$line\" >> \"$TRANSCRIPT\"\n"
       "  case \"$line\" in\n"
       "    *initialize*)\n"
       "      printf '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\\n'\n"
       "      ;;\n"
       "    *session/new*)\n"
       "      printf '{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"sessionId\":\"sess-test\"}}\\n'\n"
       "      ;;\n"
       "    *session/set_model*)\n"
       "      printf '{\"jsonrpc\":\"2.0\",\"id\":4,\"result\":{}}\\n'\n"
       "      ;;\n"
       "    *session/set_config_option*)\n"
       "      if [ \"$MODE\" = \"reject\" ]; then\n"
       "        printf "
       "'{\"jsonrpc\":\"2.0\",\"id\":5,\"error\":{\"code\":-32603,\"message\":\"rejected\"}}\\n'\n"
       "      else\n"
       "        printf '{\"jsonrpc\":\"2.0\",\"id\":5,\"result\":{}}\\n'\n"
       "      fi\n"
       "      ;;\n"
       "    *session/prompt*)\n"
       "      printf "
       "'{\"jsonrpc\":\"2.0\",\"method\":\"session/"
       "update\",\"params\":{\"sessionId\":\"sess-test\",\"update\":{\"sessionUpdate\":\"agent_"
       "message_chunk\",\"content\":{\"type\":\"text\",\"text\":\"hello\"}}}}\\n'\n"
       "      printf '{\"jsonrpc\":\"2.0\",\"id\":3,\"result\":{\"stopReason\":\"end_turn\"}}\\n'\n"
       "      ;;\n"
       "  esac\n"
       "done\n",
       tpath, mode);
   fclose(f);
   chmod(fpath, 0700);

   char cwdbuf[256];
   snprintf(cwdbuf, sizeof(cwdbuf), "%s/acp_cwd_XXXXXX", platform_tmpdir());
   char *cwd = mkdtemp(cwdbuf);
   assert(cwd != NULL);

   agent_t agent;
   memset(&agent, 0, sizeof(agent));
   snprintf(agent.name, sizeof(agent.name), "fake-acp");
   snprintf(agent.backend, sizeof(agent.backend), "%s", AGENT_BACKEND_PROVIDER_CLI);
   snprintf(agent.cli_kind, sizeof(agent.cli_kind), "acp");
   snprintf(agent.cli_cmd, sizeof(agent.cli_cmd), "%s", fpath);
   snprintf(agent.model, sizeof(agent.model), "test-model");
   if (effort)
      snprintf(agent.reasoning_effort, sizeof(agent.reasoning_effort), "%s", effort);
   agent.timeout_ms = 5000;
   agent.cli_idle_timeout_ms = 5000;

   const provider_cli_adapter_t *acp = provider_cli_adapter_get("acp");
   assert(acp != NULL);
   provider_cli_cfg_t cfg = {.agent = &agent, .cwd = cwd, .user_prompt = "hi"};
   agent_result_t out;
   memset(&out, 0, sizeof(out));
   int rc = acp->execute(&cfg, &out);

   FILE *tf2 = fopen(tpath, "rb");
   assert(tf2 != NULL);
   fseek(tf2, 0, SEEK_END);
   long sz = ftell(tf2);
   rewind(tf2);
   char *trans = malloc((size_t)sz + 1);
   assert(trans != NULL);
   size_t n = fread(trans, 1, (size_t)sz, tf2);
   trans[n] = '\0';
   fclose(tf2);

   if (expect_ok)
   {
      assert(rc == 0);
      assert(out.success == 1);
   }
   else
   {
      assert(rc != 0);
      assert(strstr(out.error, effort) != NULL);
      assert(strstr(out.error, "session/set_config_option") != NULL);
   }

   int has_effort = strstr(trans, "configId\":\"effort\"") != NULL;
   int has_prompt = strstr(trans, "session/prompt") != NULL;
   assert(has_effort == expect_has_effort);
   assert(has_prompt == expect_has_prompt);

   if (expect_has_effort)
   {
      assert(strstr(trans, "\"value\":\"xhigh\"") != NULL);
      assert(strstr(trans, "sess-test") != NULL);
      assert(strstr(trans, "\"id\":5") != NULL);
      if (expect_has_prompt)
      {
         char *p_eff = strstr(trans, "session/set_config_option");
         char *p_pr = strstr(trans, "session/prompt");
         assert(p_eff && p_pr && p_eff < p_pr);
      }
   }
   else
   {
      assert(strstr(trans, "\"id\":5") == NULL);
   }

   free(trans);
   free(out.response);
   unlink(fpath);
   unlink(tpath);
   rmdir(cwd);
}

static void test_effort_handshake(void)
{
   // accepted xhigh: effort before prompt, success
   do_effort_case("xhigh", "accept", 1, 1, 1);
   printf("PASS: accepted xhigh produces id-5 effort before prompt\n");
   // rejected xhigh: fails naming effort/method, no prompt
   do_effort_case("xhigh", "reject", 0, 1, 0);
   printf("PASS: rejected xhigh fails with effort-naming error, no prompt\n");
   // empty effort: no id-5, reaches prompt
   do_effort_case("", "accept", 1, 0, 1);
   printf("PASS: empty effort sends no id-5 and reaches prompt\n");
}

int main(void)
{
   test_acp_adapter_registered();
   test_acp_not_found_for_unknown();
   test_acp_caps();
   test_turn_text_delta();
   test_turn_session_update_string();
   test_turn_session_update_typed_object();
   test_turn_tool_call_counts();
   test_turn_result_fallback();
   test_turn_stream_beats_result();
   test_turn_standard_acp_terminal();
   test_turn_error();
   test_turn_ignores_noise();
   test_serve_write_then_read();
   test_serve_path_sandbox();
   test_serve_permission_autoapprove();
   test_serve_unknown_and_nonrequests();
   test_serve_write_denied_when_read_only();
   test_serve_permission_denied_when_read_only();
   test_effort_handshake();
   printf("ALL PASS\n");
   return 0;
}
