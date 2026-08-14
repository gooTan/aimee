/* cli_oracle.c: Oracle CLI (ChatGPT web consultation) provider adapter.
 *
 * Oracle (https://github.com/steipete/oracle) drives a signed-in ChatGPT
 * browser session (or an API key) as a one-shot question tool. aimee uses it
 * strictly as a read-only consultation seat: the complete task prompt is
 * written to a temporary file and attached with -f, and the assistant's final
 * message comes back through --write-output. Oracle never receives tools, a
 * worktree, or credentials; aimee remains the workflow controller and treats
 * the reply as text to validate, exactly like any other reviewer.
 *
 * The task travels as a file because review prompts embed complete frozen
 * diffs, which can exceed the platform's per-argument argv limit. The answer
 * travels as a file because --write-output carries only the final assistant
 * message, so parsing does not depend on Oracle's human-facing progress logs.
 *
 * Engine selection (browser vs api), remote bridging (`oracle serve` /
 * --remote-chrome), and login are operator concerns configured through
 * cli_cmd, e.g. "oracle -e browser". The adapter only appends the fixed
 * consultation shape. Sign-in is interactive and one-time:
 *   oracle --engine browser --browser-manual-login --browser-keep-browser -p "HI"
 */
#include "cli_oracle.h"

#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/select.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#include "util.h"

#define ORACLE_ARG_MAX 48
/* Pro-model runs regularly take tens of minutes; default to 45 minutes and
 * let cli_idle_timeout_ms override in either direction. */
#define ORACLE_DEFAULT_TIMEOUT_MS (45 * 60 * 1000)
#define ORACLE_STDOUT_TAIL_CAP    4096

/* The task file carries the real prompt; this argv prompt only points at it. */
#define ORACLE_TASK_INSTRUCTION                                                                    \
   "Follow the instructions in the attached aimee-task file exactly. It contains the complete "    \
   "task and the required output format. Return only what the task file specifies."

int oracle_build_argv(const provider_cli_cfg_t *cfg, const char *task_path, const char *out_path,
                      long timeout_seconds, char **tokens, int cap, int *split_count)
{
   char err[128];
   const agent_t *agent = cfg ? cfg->agent : NULL;
   const char *cmd = (agent && agent->cli_cmd[0]) ? agent->cli_cmd : "oracle";
   int count = provider_cli_split_command(cmd, tokens, cap, err, sizeof(err));
   if (count < 0)
      return -1;
   if (split_count)
      *split_count = count;

   static _Thread_local char timeout_arg[32];
   snprintf(timeout_arg, sizeof(timeout_arg), "%ld", timeout_seconds > 0 ? timeout_seconds : 60);

   int argc = count;

#define ORACLE_ADD_ARG(s)                                                                          \
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

   ORACLE_ADD_ARG("-p");
   ORACLE_ADD_ARG(ORACLE_TASK_INSTRUCTION);
   ORACLE_ADD_ARG("-f");
   ORACLE_ADD_ARG(task_path);
   ORACLE_ADD_ARG("--write-output");
   ORACLE_ADD_ARG(out_path);
   ORACLE_ADD_ARG("--no-notify");
   ORACLE_ADD_ARG("--timeout");
   ORACLE_ADD_ARG(timeout_arg);
   if (agent && agent->model[0])
   {
      ORACLE_ADD_ARG("-m");
      ORACLE_ADD_ARG(agent->model);
   }
#undef ORACLE_ADD_ARG
   return argc;
}

static const char *oracle_tmpdir(void)
{
   const char *dir = getenv("TMPDIR");
   return (dir && dir[0]) ? dir : "/tmp";
}

static int oracle_write_task_file(const provider_cli_cfg_t *cfg, char *path, size_t path_sz)
{
   snprintf(path, path_sz, "%s/aimee-oracle-task-XXXXXX", oracle_tmpdir());
   int fd = mkstemp(path);
   if (fd < 0)
      return -1;
   const char *parts[3] = {NULL, NULL, NULL};
   int n = 0;
   if (cfg->system_prompt && cfg->system_prompt[0])
   {
      parts[n++] = cfg->system_prompt;
      parts[n++] = "\n\n";
   }
   parts[n++] = cfg->user_prompt;
   for (int i = 0; i < n; i++)
   {
      size_t len = strlen(parts[i]);
      size_t written = 0;
      while (written < len)
      {
         ssize_t w = write(fd, parts[i] + written, len - written);
         if (w < 0)
         {
            if (errno == EINTR)
               continue;
            close(fd);
            unlink(path);
            return -1;
         }
         written += (size_t)w;
      }
   }
   if (close(fd) != 0)
   {
      unlink(path);
      return -1;
   }
   return 0;
}

/* Read the whole answer file. Returns a heap string (caller frees) or NULL. */
static char *oracle_read_answer(const char *path)
{
   FILE *f = fopen(path, "rb");
   if (!f)
      return NULL;
   if (fseek(f, 0, SEEK_END) != 0)
   {
      fclose(f);
      return NULL;
   }
   long size = ftell(f);
   if (size < 0 || fseek(f, 0, SEEK_SET) != 0)
   {
      fclose(f);
      return NULL;
   }
   char *content = malloc((size_t)size + 1);
   if (!content)
   {
      fclose(f);
      return NULL;
   }
   size_t got = fread(content, 1, (size_t)size, f);
   fclose(f);
   content[got] = '\0';
   return content;
}

/* Drain the child's stdout (progress logs) keeping only a bounded tail for
 * diagnostics, and enforce the wall deadline. Returns the child's exit code,
 * or -1 when the deadline killed it. */
static int oracle_wait_child(pid_t pid, int stdout_fd, long long deadline_ms, char *tail,
                             size_t tail_sz)
{
   size_t tail_len = 0;
   char buf[4096];
   int timed_out = 0;
   for (;;)
   {
      long long now = util_now_ms();
      if (now >= deadline_ms)
      {
         timed_out = 1;
         break;
      }
      long long remain = deadline_ms - now;
      struct timeval tv = {.tv_sec = (time_t)(remain / 1000),
                           .tv_usec = (suseconds_t)((remain % 1000) * 1000)};
      fd_set rfds;
      FD_ZERO(&rfds);
      FD_SET(stdout_fd, &rfds);
      int sel = select(stdout_fd + 1, &rfds, NULL, NULL, &tv);
      if (sel < 0)
      {
         if (errno == EINTR)
            continue;
         break;
      }
      if (sel == 0)
      {
         timed_out = 1;
         break;
      }
      ssize_t r = read(stdout_fd, buf, sizeof(buf));
      if (r <= 0)
         break; /* EOF: the CLI exited (or write error) */
      /* Keep the last tail_sz-1 bytes only. */
      if ((size_t)r >= tail_sz - 1)
      {
         memcpy(tail, buf + r - (ssize_t)(tail_sz - 1), tail_sz - 1);
         tail_len = tail_sz - 1;
      }
      else
      {
         if (tail_len + (size_t)r > tail_sz - 1)
         {
            size_t keep = (tail_sz - 1) - (size_t)r;
            memmove(tail, tail + tail_len - keep, keep);
            tail_len = keep;
         }
         memcpy(tail + tail_len, buf, (size_t)r);
         tail_len += (size_t)r;
      }
      tail[tail_len] = '\0';
   }
   if (timed_out)
   {
      kill(-pid, SIGTERM);
      usleep(500000);
      kill(-pid, SIGKILL);
      waitpid(pid, NULL, 0);
      return -1;
   }
   int status = 0;
   if (waitpid(pid, &status, 0) < 0)
      return -1;
   if (WIFEXITED(status))
      return WEXITSTATUS(status);
   return -1;
}

static int oracle_execute(const provider_cli_cfg_t *cfg, agent_result_t *out)
{
   memset(out, 0, sizeof(*out));
   if (!cfg || !cfg->agent)
   {
      snprintf(out->error, sizeof(out->error), "oracle adapter: missing agent config");
      return -1;
   }
   if (!cfg->user_prompt || !cfg->user_prompt[0])
   {
      snprintf(out->error, sizeof(out->error), "oracle adapter: empty prompt");
      return -1;
   }
   snprintf(out->agent_name, sizeof(out->agent_name), "%s", cfg->agent->name);

   long long start_ms = util_now_ms();
   long long timeout_ms = (cfg->agent->cli_idle_timeout_ms > 0) ? cfg->agent->cli_idle_timeout_ms
                                                                : ORACLE_DEFAULT_TIMEOUT_MS;
   long long deadline_ms = start_ms + timeout_ms;

   char task_path[512];
   if (oracle_write_task_file(cfg, task_path, sizeof(task_path)) != 0)
   {
      snprintf(out->error, sizeof(out->error), "oracle adapter: cannot write task file");
      return -1;
   }
   char out_path[512];
   snprintf(out_path, sizeof(out_path), "%s/aimee-oracle-out-XXXXXX", oracle_tmpdir());
   int out_fd = mkstemp(out_path);
   if (out_fd < 0)
   {
      unlink(task_path);
      snprintf(out->error, sizeof(out->error), "oracle adapter: cannot create answer file");
      return -1;
   }
   close(out_fd);

   char *tokens[ORACLE_ARG_MAX + 1] = {0};
   int split = 0;
   int argc = oracle_build_argv(cfg, task_path, out_path, (long)(timeout_ms / 1000), tokens,
                                ORACLE_ARG_MAX, &split);
   int rc = -1;
   if (argc < 0)
   {
      snprintf(out->error, sizeof(out->error), "oracle adapter: cannot build argv from '%s'",
               cfg->agent->cli_cmd[0] ? cfg->agent->cli_cmd : "oracle");
      goto cleanup;
   }

   int stdin_fd = -1, stdout_fd = -1;
   pid_t pid = 0;
   if (provider_cli_spawn_argv(cfg, tokens, &stdin_fd, &stdout_fd, &pid) != 0)
   {
      provider_cli_free_tokens(tokens, split);
      snprintf(out->error, sizeof(out->error), "oracle adapter: failed to spawn oracle CLI");
      goto cleanup;
   }
   provider_cli_free_tokens(tokens, split);
   if (stdin_fd >= 0)
      close(stdin_fd); /* the task travels as a file, not stdin */

   char tail[ORACLE_STDOUT_TAIL_CAP] = {0};
   int exit_code = oracle_wait_child(pid, stdout_fd, deadline_ms, tail, sizeof(tail));
   if (stdout_fd >= 0)
      close(stdout_fd);

   out->latency_ms = (int)(util_now_ms() - start_ms);
   out->turns = 1;

   char *answer = oracle_read_answer(out_path);
   if (exit_code == 0 && answer && answer[0])
   {
      out->response = answer; /* caller/agent framework frees */
      out->success = 1;
      out->confidence = 80;
      rc = 0;
      goto cleanup;
   }
   free(answer);
   if (exit_code < 0)
      snprintf(out->error, sizeof(out->error),
               "oracle adapter: timed out after %lld ms: %.400s", timeout_ms, tail);
   else
      snprintf(out->error, sizeof(out->error),
               "oracle adapter: exit %d with no answer output: %.400s", exit_code, tail);

cleanup:
   unlink(task_path);
   unlink(out_path);
   return rc;
}

const provider_cli_adapter_t oracle_provider_cli_adapter = {
    .cli_kind = "oracle",
    .display_name = "Oracle CLI",
    .caps = {.max_context_tokens = 196000,
             .supports_tool_use = 0,
             .proto_stability = PROVIDER_CLI_PROTO_STABLE,
             .write_confidence = 0.0f},
    .spawn = NULL,
    .parse_line = NULL,
    .format_tool_result = NULL,
    .is_write_event = NULL,
    .build_prompt = NULL,
    .execute = oracle_execute,
};
