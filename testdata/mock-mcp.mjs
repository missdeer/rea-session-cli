#!/usr/bin/env node
// Minimal MCP stdio server used to exercise rea-session without REA installed.
// It implements only initialize, tools/list and tools/call.

import readline from "node:readline";

const send = (value) => process.stdout.write(`${JSON.stringify(value)}\n`);

// MOCK_DELAY_MS simulates a cold provider startup in tests.
const startDelay = Number(process.env.MOCK_DELAY_MS ?? 0);

const tools = [
  {
    name: "open_binary",
    description: "Open a target (mock)",
    inputSchema: {
      type: "object",
      properties: { path: { type: "string" } },
      required: ["path"],
    },
  },
  {
    name: "binary_session",
    description: "Report the active session (mock)",
    inputSchema: { type: "object", properties: {} },
  },
  {
    name: "close_binary",
    description: "Close the active target (mock)",
    inputSchema: { type: "object", properties: {} },
  },
];

readline.createInterface({ input: process.stdin }).on("line", (line) => {
  let message;
  try {
    message = JSON.parse(line);
  } catch {
    return;
  }
  if (message.method === "initialize") {
    const reply = () =>
      send({
        jsonrpc: "2.0",
        id: message.id,
        result: {
          protocolVersion: "2025-06-18",
          capabilities: { tools: {} },
          serverInfo: { name: "mock-rea", version: "0.0.0" },
        },
      });
    if (startDelay > 0) {
      setTimeout(reply, startDelay);
      return;
    }
    return reply();
  }
  if (message.method === "notifications/initialized") return;
  if (message.method === "tools/list") {
    return send({ jsonrpc: "2.0", id: message.id, result: { tools } });
  }
  if (message.method === "tools/call") {
    const name = message.params?.name;
    const args = message.params?.arguments ?? {};
    const reply = (structuredContent) =>
      send({
        jsonrpc: "2.0",
        id: message.id,
        result: {
          content: [{ type: "text", text: JSON.stringify(structuredContent) }],
          structuredContent,
        },
      });
    if (name === "open_binary") return reply({ active: true, path: args.path });
    if (name === "binary_session") return reply({ active: true, tools: tools.length });
    if (name === "close_binary") return reply({ active: false });
    return send({
      jsonrpc: "2.0",
      id: message.id,
      result: {
        content: [{ type: "text", text: `unknown tool ${name}` }],
        isError: true,
      },
    });
  }
  if (message.id !== undefined) {
    send({
      jsonrpc: "2.0",
      id: message.id,
      error: { code: -32601, message: "method not found" },
    });
  }
});
