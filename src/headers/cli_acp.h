#ifndef DEC_CLI_ACP_H
#define DEC_CLI_ACP_H 1

#include <stddef.h>

/* acp_turn_state_t: accumulator for one outbound ACP turn's streamed events. */
typedef struct
{
   char *text;     /* heap-accumulated reply text (caller frees)            */
   size_t len;     /* length of text                                        */
   int tool_calls; /* number of tool-call events observed this turn         */
   int done;       /* set once the terminal prompt response arrives         */
   int had_error;  /* set when an error response arrives                    */
   int prompt_id;  /* JSON-RPC id of the prompt request whose response ends  */
                   /* the turn; 0 means the legacy default of 2             */
   char error[256];
} acp_turn_state_t;

/* Consume one JSON-RPC line emitted by an outbound ACP agent into `st`.
 * Understands both aimee's legacy dialect (text/delta, tool/call) and standard
 * ACP session/update notifications (string or {type,text} content, and
 * tool_call updates), plus the terminal result(id==2)/error responses. A blank
 * or unparseable line is ignored. Returns 0 normally, -1 if the line was an
 * error response (st->had_error and st->error populated). */
int acp_turn_consume(const char *line, acp_turn_state_t *st);

/* Serve a client-side ACP request the agent issues during a turn (fs read/write
 * against the worktree, permission prompts). `workdir` sandboxes every path.
 * Returns 1 when `line` was an agent request and *resp_out holds the heap
 * JSON-RPC response to send back (caller frees); 0 when `line` is not a request
 * (no id, or a response) and should be handled as turn content via
 * acp_turn_consume. */
int acp_serve_client_request(const char *line, const char *workdir, char **resp_out);

/* Write-gated variant. When write_capable is 0, fs/write_text_file is refused
 * and session/request_permission selects a reject/deny option (or cancels)
 * instead of auto-approving, so a read-only role dispatched over ACP cannot
 * mutate the worktree even if the agent asks nicely. The ungated wrapper above
 * behaves as write_capable=1 for compatibility. */
int acp_serve_client_request_gated(const char *line, const char *workdir, int write_capable,
                                   char **resp_out);

#endif /* DEC_CLI_ACP_H */
