/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { useTranslation } from 'react-i18next'

export function useAnalysisLabels(): Record<string, string> {
  const { t } = useTranslation()
  return {
    settlement_risk: t('Settlement risk'),
    upstream_usage_unbilled: t('Upstream usage without local consumption'),
    charged_zero_output: t('Charged with zero output'),
    confirmed: t('Funding settlement confirmed'),
    unconfirmed: t('Settlement not confirmed'),
    zero_output: t('Zero output'),
    interrupted: t('Interrupted streams'),
    error: t('Request errors'),
    client_cancelled: t('Client cancelled'),
    unknown_output: t('Insufficient output evidence'),
    recovered: t('Recovered after retry'),
    pending: t('Queued'),
    running: t('Running'),
    succeeded: t('Completed'),
    failed: t('Failed'),
    collecting: t('Collecting evidence'),
    analyzing: t('Analyzing segments'),
    merging: t('Combining findings'),
    writing: t('Writing report sections'),
    summarizing: t('Preparing the summary'),
    paused: t('Paused'),
    retrying: t('Waiting to retry'),
    completed: t('Completed'),
    finish_stop: t('Completed'),
    finish_length: t('Output token limit reached'),
    finish_content_filter: t('Content filtered'),
    finish_tool_calls: t('Tool call requested'),
    finish_function_call: t('Function call requested'),
    finish_insufficient_system_resource: t('Provider resource limit'),
    finish_aborted: t('Generation aborted'),
    analysis_http_error: t('Request failed'),
    analysis_input_error: t('Request failed'),
    analysis_invalid_range: t('Invalid analysis time range or filters'),
    analysis_idle_timeout: t(
      'The model stopped producing tokens. Check provider latency or increase the no-token timeout.'
    ),
    analysis_timeout: t(
      'The model exceeded the attempt timeout. Check its latency or increase the timeout.'
    ),
    analysis_authentication_failed: t(
      'The provider rejected the API key. Check its validity and model permissions.'
    ),
    analysis_rate_limited: t(
      'The provider rate limit was reached. Wait or adjust provider capacity.'
    ),
    analysis_invalid_request: t(
      'The provider rejected this request. Check the endpoint and model name.'
    ),
    analysis_stream_interrupted: t(
      'The model stream ended before completion. Check provider and network stability.'
    ),
    analysis_stream_error: t(
      'The provider returned an error inside the stream.'
    ),
    analysis_empty_report: t('The model completed without report content.'),
    analysis_incomplete_report: t(
      'The report was truncated or blocked by the provider.'
    ),
    analysis_not_sse: t(
      'The endpoint did not return an SSE stream. Check Chat Completions compatibility.'
    ),
    analysis_invalid_sse: t('The provider returned an invalid SSE event.'),
    analysis_connection_failed: t(
      'Unable to connect to the provider. Check DNS, TLS and endpoint accessibility.'
    ),
    analysis_response_too_large: t(
      'The model response exceeded the safe size limit.'
    ),
    analysis_key_unavailable: t(
      'The saved key is unavailable. Check CRYPTO_SECRET or save the API key again.'
    ),
    analysis_configuration_error: t(
      'The analysis configuration is invalid. Review the settings.'
    ),
    analysis_log_query_failed: t(
      'Unable to collect logs. Check database availability or choose a shorter range.'
    ),
    analysis_persistence_error: t(
      'Unable to save analysis progress. Check database health.'
    ),
    analysis_cancelled: t(
      'Analysis was cancelled or its worker lease was lost.'
    ),
    analysis_internal_error: t(
      'The analysis worker failed. Review server logs before retrying.'
    ),
    analysis_call_budget_exhausted: t(
      'The model call budget was reached. Completed results have been saved.'
    ),
    analysis_context_limit: t(
      'The request exceeded the model context limit. Review the context settings.'
    ),
    analysis_invalid_response: t('The provider returned an invalid response.'),
    analysis_invalid_finding: t(
      'The model returned findings that could not be verified.'
    ),
    analysis_incomplete_coverage: t(
      'Some analysis data is incomplete. Review coverage before drawing conclusions.'
    ),
    analysis_budget_too_small: t(
      'The token budget is too small for the required instructions and evidence.'
    ),
    analysis_evidence_unavailable: t('Saved analysis evidence is unavailable.'),
    analysis_resume_unavailable: t(
      'This analysis cannot be resumed. Start a new analysis.'
    ),
    analysis_source_changed: t(
      'The log database changed before collection finished. Start a new analysis.'
    ),
    cross_filter_retry_evidence_incomplete: t(
      'Retries on other models or channels may be missing from this filtered analysis.'
    ),
    clickhouse_snapshot_engine_unverified: t(
      'This log storage engine cannot guarantee a stable analysis snapshot.'
    ),
    source_rows_changed_during_scan: t(
      'Log records changed during collection. This report has incomplete coverage.'
    ),
    'task lease expired': t(
      'The worker stopped responding. Start a new analysis to recover.'
    ),
  }
}
