import type { Register } from 'claude-code'

const PREFIX = 'mcp__agentparley__'
const POLL_SECONDS = 25
const BRIDGE_DEAD_MS = 30_000

const COMMAND = { seed: 'seed', submit: 'submit', completePart: 'complete-part', complete: 'complete' } as const
const STEP_MODE_MADE_UP = 'made-up'
const EVENT = { blockedTool: 'blocked_tool', error: 'error' } as const
const REFUSAL_REASON = 'refusal'

type Reply = { status: number; text: string }
type Json = Record<string, any>

let bridge = ''
let isStopped = false
let failingSince: number | null = null
const ranSeqs = new Set<number>()
const promptText = { system: '', toolNote: '' }
const completeParts = new Map<string, string>()

async function send($: any, method: string, path: string, body?: Json): Promise<Reply> {
  const init: Json = { method, socketPath: bridge }
  if (body !== undefined) {
    init.body = JSON.stringify(body)
    init.headers = { 'content-type': 'application/json' }
  }
  const reply = await $.http.fetch('http://agentparley' + path, init)
  failingSince = null
  return { status: reply.status, text: reply.text }
}

// Claude Code outlives a daemon that died without closing it; once the socket has been dead this long nobody is
// coming back, and SIGTERM is the documented way a session ends from outside ($.process has no exit of its own).
async function noteBridgeFault($: any, where: string, error: unknown): Promise<void> {
  $.ui.log('agentparley bridge ' + where + ': ' + String(error), { to: 'debug' })
  const now: number = await $.clock.now()
  if (failingSince === null) {
    failingSince = now
    return
  }
  if (now - failingSince >= BRIDGE_DEAD_MS) {
    isStopped = true
    await $.process.run(['sh', '-c', 'kill -TERM "$PPID"']).catch(() => undefined)
  }
}

async function post($: any, path: string, body: Json): Promise<Json> {
  const reply = await send($, 'POST', path, body)
  if (reply.status === 409) {
    isStopped = true
  }
  return reply.text ? JSON.parse(reply.text) : {}
}

async function reportEvent($: any, kind: string, fields: Json): Promise<void> {
  try {
    await post($, '/event', { kind, ...fields })
  } catch (error) {
    await noteBridgeFault($, 'event', error)
  }
}

function guardSlash(text: string): string {
  return text.trimStart().startsWith('/') ? 'Message: ' + text : text
}

async function runCommand($: any, command: Json): Promise<void> {
  switch (command.kind) {
    case COMMAND.seed: {
      await $.session.append({ message: { type: 'user', content: [{ type: 'text', text: command.text }] } })
      return
    }
    case COMMAND.submit: {
      $.prompt.submit({ text: guardSlash(command.text), asUser: true }).catch((error: unknown) => {
        void reportEvent($, EVENT.error, { error: 'invalid_request', detail: 'the prompt was not accepted: ' + String(error) })
      })
      return
    }
    case COMMAND.completePart: {
      completeParts.set(command.id, (completeParts.get(command.id) ?? '') + command.text)
      return
    }
    case COMMAND.complete: {
      const prompt = (completeParts.get(command.id) ?? '') + command.prompt
      completeParts.delete(command.id)
      const request: Json = { model: command.model, prompt, maxTokens: command.maxTokens }
      if (command.system) {
        request.system = command.system
      }
      if (command.effort) {
        request.effort = command.effort
      }
      void $.model.complete(request).then(
        (result: Json) => post($, '/complete-result', { id: command.id, ...result }),
        (error: unknown) => post($, '/complete-result', { id: command.id, isAnswered: false, reason: 'failed', detail: String(error) }),
      )
      return
    }
  }
}

function schedule($: any): void {
  if (isStopped) {
    return
  }
  $.clock.after(0, () => {
    void tick($)
  })
}

async function tick($: any): Promise<void> {
  try {
    const reply = await send($, 'GET', '/next?wait=' + POLL_SECONDS)
    if (reply.status === 200) {
      const command: Json = JSON.parse(reply.text)
      if (!ranSeqs.has(command.seq)) {
        await runCommand($, command)
        ranSeqs.add(command.seq)
      }
      await post($, '/ack', { seq: command.seq })
    } else if (reply.status !== 204) {
      await $.clock.sleep(1000)
    }
  } catch (error) {
    await noteBridgeFault($, 'next', error)
    await $.clock.sleep(1000)
  }
  schedule($)
}

// Input that does not parse is forwarded as the raw string: it is not an object, so Platform answers the model with
// invalid_arguments_json instead of running a call the model never made.
function parseInput(json: string): unknown {
  try {
    return json === '' ? {} : JSON.parse(json)
  } catch {
    return json
  }
}

function isOurs(tool: string): boolean {
  return tool.startsWith(PREFIX)
}

async function awaitResult($: any, callId: string, signal: AbortSignal): Promise<Json> {
  while (!signal.aborted && !isStopped) {
    try {
      const reply = await send($, 'GET', '/result?callId=' + encodeURIComponent(callId) + '&wait=' + POLL_SECONDS)
      if (reply.status === 200) {
        return JSON.parse(reply.text)
      }
      if (reply.status !== 204) {
        return { deny: 'AgentParley could not deliver this tool result.' }
      }
    } catch (error) {
      await noteBridgeFault($, 'result', error)
      await $.clock.sleep(1000)
    }
  }
  return { deny: 'The tool call was cancelled.' }
}

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    bridge = (await $.env.get('AGENTPARLEY_BRIDGE')) ?? ''
    isStopped = false
    failingSince = null
    ranSeqs.clear()
    const version = (await $.session.version()).version
    const before = (await $.tool.list()).map(t => t.name)
    const hello = await post($, '/hello', { engineVersion: version, toolsBefore: before })
    for (const tool of hello.tools ?? []) {
      await $.tool.register({ name: tool.name, description: tool.description, inputSchema: tool.inputSchema })
    }
    promptText.system = hello.system ?? ''
    promptText.toolNote = hello.toolNote ?? ''
    await post($, '/tools-registered', { tools: (await $.tool.list()).map(t => t.name) })
    schedule($)
    return next(e)
  })

  on('prompt.compose', () => {
    const sections: { id: string; text: string; scope: 'session' }[] = []
    if (promptText.system) {
      sections.push({ id: 'agentparley:system', text: promptText.system, scope: 'session' })
    }
    if (promptText.toolNote) {
      sections.push({ id: 'agentparley:tool-names', text: promptText.toolNote, scope: 'session' })
    }
    return { sections }
  })

  on('prompt.context', () => ({ blocks: [] }))

  on('tool.describe', async ($, e, next) => {
    const described = await next(e)
    return isOurs(e.tool) ? { ...described, isDeferred: false } : described
  })

  on('tool.check', ($, e) => {
    if (isOurs(e.tool)) {
      return { decision: 'allow' }
    }
    return { decision: 'deny', reason: 'Only AgentParley tools are available.' }
  }).catch(() => ({ decision: 'deny', reason: 'Only AgentParley tools are available.' }))

  on('tool.call', async ($, e, next) => {
    if (!isOurs(e.tool)) {
      await reportEvent($, EVENT.blockedTool, { tool: e.tool, detail: e.tool + ' was called' })
      return { deny: 'Only AgentParley tools are available in this session.' }
    }
    const answer = await awaitResult($, e.tool_use_id, next.signal)
    if (answer.deny !== undefined) {
      return { deny: answer.deny }
    }
    return answer.context && answer.context.length > 0
      ? { result: answer.text, context: answer.context }
      : { result: answer.text }
  }).catch(() => ({ deny: 'Only AgentParley tools are available in this session.' }))

  on('command.run', async ($, e) => {
    await reportEvent($, EVENT.blockedTool, { tool: '/' + e.command, detail: 'a slash command was attempted' })
    return { text: '' }
  }).catch(() => ({ text: '' }))

  on('agent.offer', () => ({ isOffered: false })).catch(() => ({ isOffered: false }))

  on('turn.step', async function* ($, e, next) {
    if (e.agentId !== undefined) {
      return yield* next(e)
    }
    const decision = await post($, '/step-start', { turnId: e.turnId, index: e.index })
    if (decision.mode === STEP_MODE_MADE_UP) {
      const toolUses: { name: string; input: unknown }[] = decision.toolUses ?? []
      const ids: string[] = decision.toolIds ?? []
      let block = 0
      if (decision.text) {
        yield { kind: 'text', index: block, text: decision.text }
        block += 1
      }
      for (let at = 0; at < toolUses.length; at += 1) {
        const use = toolUses[at]!
        yield { kind: 'tool', index: block, id: ids[at] ?? 'toolu_made_up_' + at, name: use.name }
        yield { kind: 'input', index: block, json: JSON.stringify(use.input ?? {}) }
        block += 1
      }
      yield { kind: 'stop', stopReason: decision.stopReason, usage: null }
      return {
        turnId: e.turnId,
        index: e.index,
        answer: decision.text ?? '',
        toolUses,
        stopReason: decision.stopReason,
        usage: null,
      }
    }

    const texts = new Map<number, string>()
    const tools = new Map<number, { id: string; name: string; json: string }>()
    let thinking = ''
    const stream = next(e)
    for (;;) {
      const step = await stream.next()
      if (step.done) {
        return step.value
      }
      const chunk = step.value
      if (chunk.kind === 'text') {
        texts.set(chunk.index, (texts.get(chunk.index) ?? '') + chunk.text)
      } else if (chunk.kind === 'thinking') {
        thinking += chunk.text
      } else if (chunk.kind === 'tool') {
        tools.set(chunk.index, { id: chunk.id, name: chunk.name, json: '' })
      } else if (chunk.kind === 'input') {
        const tool = tools.get(chunk.index)
        if (tool !== undefined) {
          tool.json += chunk.json
        }
      } else if (chunk.kind === 'stop') {
        await post($, '/step-end', {
          turnId: e.turnId,
          index: e.index,
          text: [...texts.entries()].sort((a, b) => a[0] - b[0]).map(entry => entry[1]).join(''),
          thinking,
          toolUses: [...tools.entries()].sort((a, b) => a[0] - b[0]).map(entry => ({
            id: entry[1].id,
            name: entry[1].name,
            input: parseInput(entry[1].json),
          })),
          stopReason: chunk.stopReason,
          usage: chunk.usage,
        })
      }
      yield chunk
    }
  })

  on('turn.complete', async ($, e, next) => {
    await post($, '/turn-complete', {
      turnId: e.turnId,
      answer: e.answer,
      reason: e.reason,
      refusal: e.reason === REFUSAL_REASON ? e.refusal : null,
    })
    return next(e)
  })

  on('classic.StopFailure', async ($, e, next) => {
    await reportEvent($, EVENT.error, { error: e.error, detail: e.error_details ?? '' })
    return next(e)
  }).catch(($, e, next) => next(e))
}
