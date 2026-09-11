#!/usr/bin/env node
// Small ACP peer used by buzz-runner-integration.mjs. It is intentionally
// disposable: it never connects to a relay and only speaks JSON-RPC on stdio.
let buffer = '';
let permissionID = 40;
let sessionID = 'qa-session';
let awaitingPermission = false;
if (process.env.FAKE_ACP_MODE === 'crash-once') {
  const fs = await import('node:fs');
  if (!fs.existsSync(process.env.FAKE_ACP_MARKER)) fs.writeFileSync(process.env.FAKE_ACP_MARKER, 'crashed');
  else process.env.FAKE_ACP_MODE = '';
}
process.stdin.setEncoding('utf8');
process.stdin.on('data', chunk => {
  buffer += chunk;
  while (buffer.includes('\n')) {
    const newline = buffer.indexOf('\n');
    const line = buffer.slice(0, newline);
    buffer = buffer.slice(newline + 1);
    if (!line.trim()) continue;
    let frame;
    try { frame = JSON.parse(line); } catch { continue; }
    handle(frame);
  }
});
function send(frame) { process.stdout.write(JSON.stringify(frame) + '\n'); }
function handle(frame) {
  if (frame.method === 'initialize') {
    send({jsonrpc: '2.0', id: frame.id, result: {protocolVersion: 1}});
    return;
  }
  if (frame.method === 'session/new') {
    send({jsonrpc: '2.0', id: frame.id, result: {sessionId: sessionID}});
    return;
  }
  if (frame.method === 'session/cancel') {
    send({jsonrpc: '2.0', id: frame.id, result: {}});
    return;
  }
  if (frame.method === 'session/prompt') {
    if (process.env.FAKE_ACP_MODE === 'crash-once') process.exit(23);
    send({jsonrpc: '2.0', method: 'session/update', params: {sessionId: sessionID, update: {sessionUpdate: 'tool_call', title: 'publish preview', status: 'running'}}});
    awaitingPermission = true;
    send({jsonrpc: '2.0', id: ++permissionID, method: 'session/request_permission', params: {sessionId: sessionID, toolCall: {title: 'Publish preview', rawInput: {path: 'public/preview.html'}}, options: [{optionId: 'allow', name: 'Allow once', kind: 'allow_once'}, {optionId: 'deny', name: 'Deny once', kind: 'reject_once'}]}});
    return;
  }
  if (frame.id === permissionID && awaitingPermission) {
    awaitingPermission = false;
    const outcome = frame.result?.outcome;
    if (outcome?.outcome !== 'selected' || outcome.optionId !== 'allow') {
      send({jsonrpc: '2.0', id: 3, result: {stopReason: 'cancelled'}});
      return;
    }
    send({jsonrpc: '2.0', method: 'session/update', params: {sessionId: sessionID, update: {sessionUpdate: 'agent_message_chunk', content: {type: 'text', text: 'Preview published.'}}}});
    send({jsonrpc: '2.0', id: 3, result: {stopReason: 'end_turn'}});
  }
}
