// --- WebSocket Frame Protocol ---
//
// Frame format: [1-byte channel][4-byte length (big-endian)][payload]
// Channels: 0=control, 1=PTY, 2=chat

export const Channel = {
  Control: 0,
  PTY: 1,
  Chat: 2,
} as const;

export type ChannelId = (typeof Channel)[keyof typeof Channel];

// --- Control Message Types ---

export interface ControlRegister {
  type: 'register';
  daemonId: string;
  repos: string[]; // repo full names (owner/repo)
}

export interface ControlHeartbeat {
  type: 'heartbeat';
  timestamp: number;
}

export interface ControlEvent {
  type: 'event';
  eventId: string;
  payload: unknown;
}

export interface ControlAck {
  type: 'ack';
  eventId: string;
}

export type ControlMessage = ControlRegister | ControlHeartbeat | ControlEvent | ControlAck;

// --- Frame ---

export interface Frame {
  channel: ChannelId;
  payload: Uint8Array;
}

const HEADER_SIZE = 5; // 1 byte channel + 4 bytes length

export function encodeFrame(channel: ChannelId, payload: Uint8Array): Uint8Array {
  const frame = new Uint8Array(HEADER_SIZE + payload.length);
  frame[0] = channel;
  const view = new DataView(frame.buffer, frame.byteOffset, frame.byteLength);
  view.setUint32(1, payload.length, false); // big-endian
  frame.set(payload, HEADER_SIZE);
  return frame;
}

export function decodeFrame(data: Uint8Array): Frame {
  if (data.length < HEADER_SIZE) {
    throw new Error(`Frame too short: ${data.length} bytes`);
  }
  const channel = data[0] as ChannelId;
  const view = new DataView(data.buffer, data.byteOffset, data.byteLength);
  const length = view.getUint32(1, false);
  if (data.length < HEADER_SIZE + length) {
    throw new Error(
      `Frame payload incomplete: expected ${length} bytes, got ${data.length - HEADER_SIZE}`,
    );
  }
  const payload = data.slice(HEADER_SIZE, HEADER_SIZE + length);
  return { channel, payload };
}

// --- Helpers ---

const encoder = new TextEncoder();
const decoder = new TextDecoder();

export function encodeControlMessage(msg: ControlMessage): Uint8Array {
  const json = encoder.encode(JSON.stringify(msg));
  return encodeFrame(Channel.Control, json);
}

export function decodeControlMessage(frame: Frame): ControlMessage {
  if (frame.channel !== Channel.Control) {
    throw new Error(`Expected control channel (0), got channel ${frame.channel}`);
  }
  return JSON.parse(decoder.decode(frame.payload)) as ControlMessage;
}
