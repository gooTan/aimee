/* db1_optional.h: DB1 domain calls that may be absent from DB2-only
 * binaries such as aimee-kb.
 *
 * Server builds link the real DB1 objects, so these weak references resolve.
 * KB builds intentionally do not link DB1; callers must guard optional calls
 * before invoking them. */
#ifndef DEC_DB1_OPTIONAL_H
#define DEC_DB1_OPTIONAL_H 1

#include "db1.h"

#if defined(AIMEE_DB1_DISABLED)
#define DB1_OPTIONAL_NULL(name)                               ((__typeof__(name) *)0)
#define db1_agent_log_list_delegation_patterns                DB1_OPTIONAL_NULL(db1_agent_log_list_delegation_patterns)
#define db1_agent_log_list_failure_episode_seeds              DB1_OPTIONAL_NULL(db1_agent_log_list_failure_episode_seeds)
#define db1_agent_log_list_recent_errors                      DB1_OPTIONAL_NULL(db1_agent_log_list_recent_errors)
#define db1_cognify_job_claim_next                            DB1_OPTIONAL_NULL(db1_cognify_job_claim_next)
#define db1_cognify_job_enqueue                               DB1_OPTIONAL_NULL(db1_cognify_job_enqueue)
#define db1_cognify_job_mark                                  DB1_OPTIONAL_NULL(db1_cognify_job_mark)
#define db1_cognify_job_status                                DB1_OPTIONAL_NULL(db1_cognify_job_status)
#define db1_context_cache_get                                 DB1_OPTIONAL_NULL(db1_context_cache_get)
#define db1_context_cache_invalidate                          DB1_OPTIONAL_NULL(db1_context_cache_invalidate)
#define db1_context_cache_put                                 DB1_OPTIONAL_NULL(db1_context_cache_put)
#define db1_context_snapshot_count_for_memory                 DB1_OPTIONAL_NULL(db1_context_snapshot_count_for_memory)
#define db1_context_snapshot_count_memories_with_min_samples  DB1_OPTIONAL_NULL(db1_context_snapshot_count_memories_with_min_samples)
#define db1_context_snapshot_has_memory                       DB1_OPTIONAL_NULL(db1_context_snapshot_has_memory)
#define db1_context_snapshot_insert                           DB1_OPTIONAL_NULL(db1_context_snapshot_insert)
#define db1_context_snapshot_list_memory_ids_with_min_samples DB1_OPTIONAL_NULL(db1_context_snapshot_list_memory_ids_with_min_samples)
#define db1_context_snapshot_list_sessions_for_memory         DB1_OPTIONAL_NULL(db1_context_snapshot_list_sessions_for_memory)
#define db1_decision_record                                   DB1_OPTIONAL_NULL(db1_decision_record)
#define db1_default_path                                      DB1_OPTIONAL_NULL(db1_default_path)
#define db1_maintenance_state_load                            DB1_OPTIONAL_NULL(db1_maintenance_state_load)
#define db1_maintenance_state_save                            DB1_OPTIONAL_NULL(db1_maintenance_state_save)
#define db1_runtime_state_add_int                             DB1_OPTIONAL_NULL(db1_runtime_state_add_int)
#define db1_runtime_state_get                                 DB1_OPTIONAL_NULL(db1_runtime_state_get)
#define db1_runtime_state_set                                 DB1_OPTIONAL_NULL(db1_runtime_state_set)
#define db1_server_session_get                                DB1_OPTIONAL_NULL(db1_server_session_get)
#define db1_window_add_file                                   DB1_OPTIONAL_NULL(db1_window_add_file)
#define db1_window_add_term                                   DB1_OPTIONAL_NULL(db1_window_add_term)
#define db1_window_create_raw                                 DB1_OPTIONAL_NULL(db1_window_create_raw)
#define db1_window_delete_all_files                           DB1_OPTIONAL_NULL(db1_window_delete_all_files)
#define db1_window_index_summary                              DB1_OPTIONAL_NULL(db1_window_index_summary)
#define db1_window_list_files                                 DB1_OPTIONAL_NULL(db1_window_list_files)
#define db1_window_prune_files_keep_top                       DB1_OPTIONAL_NULL(db1_window_prune_files_keep_top)
#define db1_window_prune_terms_keep_top                       DB1_OPTIONAL_NULL(db1_window_prune_terms_keep_top)
#define db1_window_session_id                                 DB1_OPTIONAL_NULL(db1_window_session_id)
#define db1_window_set_tier                                   DB1_OPTIONAL_NULL(db1_window_set_tier)
#define db1_windows_find_candidates_by_terms                  DB1_OPTIONAL_NULL(db1_windows_find_candidates_by_terms)
#define db1_windows_find_lexical_hits                         DB1_OPTIONAL_NULL(db1_windows_find_lexical_hits)
#define db1_windows_list_ids_by_tier_before_days              DB1_OPTIONAL_NULL(db1_windows_list_ids_by_tier_before_days)
#define db1_windows_session_scan_state                        DB1_OPTIONAL_NULL(db1_windows_session_scan_state)
#elif defined(__GNUC__) || defined(__clang__)
#pragma weak db1_agent_log_list_delegation_patterns
#pragma weak db1_agent_log_list_failure_episode_seeds
#pragma weak db1_agent_log_list_recent_errors
#pragma weak db1_cognify_job_claim_next
#pragma weak db1_cognify_job_enqueue
#pragma weak db1_cognify_job_mark
#pragma weak db1_cognify_job_status
#pragma weak db1_context_cache_get
#pragma weak db1_context_cache_invalidate
#pragma weak db1_context_cache_put
#pragma weak db1_context_snapshot_count_for_memory
#pragma weak db1_context_snapshot_count_memories_with_min_samples
#pragma weak db1_context_snapshot_has_memory
#pragma weak db1_context_snapshot_insert
#pragma weak db1_context_snapshot_list_memory_ids_with_min_samples
#pragma weak db1_context_snapshot_list_sessions_for_memory
#pragma weak db1_decision_record
#pragma weak db1_default_path
#pragma weak db1_maintenance_state_load
#pragma weak db1_maintenance_state_save
#pragma weak db1_runtime_state_add_int
#pragma weak db1_runtime_state_get
#pragma weak db1_runtime_state_set
#pragma weak db1_server_session_get
#pragma weak db1_window_add_file
#pragma weak db1_window_add_term
#pragma weak db1_window_create_raw
#pragma weak db1_window_delete_all_files
#pragma weak db1_window_index_summary
#pragma weak db1_window_list_files
#pragma weak db1_window_prune_files_keep_top
#pragma weak db1_window_prune_terms_keep_top
#pragma weak db1_window_session_id
#pragma weak db1_window_set_tier
#pragma weak db1_windows_find_candidates_by_terms
#pragma weak db1_windows_find_lexical_hits
#pragma weak db1_windows_list_ids_by_tier_before_days
#pragma weak db1_windows_session_scan_state
#endif

#endif /* DEC_DB1_OPTIONAL_H */
