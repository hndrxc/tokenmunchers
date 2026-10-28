# Metadata allowlist used to capture redacted real Claude Code transcripts (see MANIFEST.md).
# Prompts, attachments, request blobs, tool inputs/results and assistant content are never copied.
if .type == "assistant" then
  { type, timestamp, uuid, parentUuid, sessionId, isSidechain, requestId, version,
    message: { id: .message.id, model: .message.model, stop_reason: .message.stop_reason, usage: .message.usage } }
elif .type == "cost-state" then
  { type, sessionId, startTime, totalCostUSD, totalAPIDuration, totalAPIDurationWithoutRetries, totalToolDuration,
    totalDuration, totalLinesAdded, totalLinesRemoved, hasUnknownModelCost, modelUsage }
else
  { type } + ({ timestamp, uuid, parentUuid, sessionId, isSidechain } | with_entries(select(.value != null)))
end
