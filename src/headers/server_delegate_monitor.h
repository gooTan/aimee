/* server_delegate_monitor.h: delegate runtime heartbeat and stale-state inspection.
 *
 * Enabled by default. Set AIMEE_DELEGATE_HEARTBEAT_MONITOR=0 to disable
 * the monitor while debugging delegate runtime behavior. */
#ifndef DEC_SERVER_DELEGATE_MONITOR_H
#define DEC_SERVER_DELEGATE_MONITOR_H 1

#ifdef __cplusplus
extern "C"
{
#endif

   /* Idempotent. If AIMEE_DELEGATE_HEARTBEAT_MONITOR=0, no thread starts
    * and this is a no-op. Safe to call from server_init unconditionally. */
   void server_delegate_monitor_init(void);

   /* Idempotent. Joins the monitor thread (if started). Safe to call from
    * server_shutdown unconditionally. */
   void server_delegate_monitor_shutdown(void);

   /* Test seam: classify every running agent_job without changing it.
    * Returns the number suspected stalled. */
   int server_delegate_monitor_sweep(int idle_threshold_secs, int in_tool_threshold_secs);

   /* Bind/unbind a per-turn heartbeat for the in-flight background delegate on
    * THIS thread. begin() registers an http_retry progress callback that bumps
    * agent_jobs.heartbeat_at after every model HTTP attempt, so a slow-but-
    * progressing delegate (each turn bounded by the agent timeout, under the
    * stale idle threshold) is not auto-cancelled as stalled — the reason only the
    * fastest model used to survive the fleet. job_id <= 0 is a no-op. Always pair
    * begin() with end(). */
   void server_delegate_heartbeat_begin(int job_id);
   void server_delegate_heartbeat_end(void);

#ifdef __cplusplus
}
#endif

#endif /* DEC_SERVER_DELEGATE_MONITOR_H */
