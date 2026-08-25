#ifndef DEC_CLI_ORACLE_H
#define DEC_CLI_ORACLE_H 1

#include "provider_cli_adapter.h"

/* Build the oracle argv: parsed cli_cmd (default "oracle"), then the fixed
 * consultation shape: a short instruction prompt, the task file attachment,
 * --write-output to the answer file, --no-notify, --timeout seconds, and the
 * pinned model when the agent names one. The first *split_count tokens are
 * heap-allocated; free with provider_cli_free_tokens. Returns argc or -1.
 * Exposed for tests. */
int oracle_build_argv(const provider_cli_cfg_t *cfg, const char *task_path, const char *out_path,
                      long timeout_seconds, char **tokens, int cap, int *split_count);

#endif /* DEC_CLI_ORACLE_H */
