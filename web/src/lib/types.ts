/**
 * Core type definitions matching api/openapi.yaml
 */

export type StreamMode = "pcm" | "opus";
export type DeviceState = "serving" | "skipped" | "failed" | "disabled";

export interface OpusSettings {
  bitrate?: number;
}

export interface DeviceConfig {
  name: string;
  device: string;
  path: string;
  mode: StreamMode;
  rate: number;
  // Selected 1-based capture channel numbers to stream, ascending and unique
  // (e.g. [1], [1, 2], or [1, 3]). A single-channel selection is a mono stream.
  channels: number[];
  format: "s16";
  opus?: OpusSettings;
  // Whether the device is captured and streamed; defaults to true when absent.
  // A disabled device stays configured but is not opened until re-enabled and
  // the appliance restarts.
  enabled?: boolean;
}

export interface DiscoverySettings {
  enabled?: boolean;
}

export interface ManagementSettings {
  enabled?: boolean;
  listen?: string;
  certDir?: string;
}

// AuthSettings is the shared access token that gates the management API and
// web UI (bearer) and the RTSP stream (Digest password). An empty token means
// open access; in a patch, an absent field leaves it unchanged and an empty
// string disables authentication.
export interface AuthSettings {
  token?: string;
}

export interface Config {
  listen: string;
  discovery?: DiscoverySettings;
  management?: ManagementSettings;
  auth?: AuthSettings;
  devices: DeviceConfig[];
}

// Only discovery, auth and devices are patchable; the server ignores anything
// else (see api/openapi.yaml ConfigPatch).
export interface ConfigPatch {
  discovery?: DiscoverySettings;
  auth?: AuthSettings;
  devices?: DeviceConfig[];
}

export interface ConfigUpdateResult {
  config: Config;
  restartRequired: boolean;
}

export interface ApplianceStatus {
  version: string;
  uptimeSeconds: number;
  rtspListen: string;
  discoveryEnabled: boolean;
  // Whether a shared access token is configured (the API, UI and RTSP stream
  // require credentials).
  authRequired: boolean;
  devicesServing: number;
  devicesTotal: number;
}

export interface Device {
  name: string;
  device: string;
  path: string;
  mode: StreamMode;
  format: "s16";
  rate: number;
  // Selected 1-based capture channel numbers streamed (see DeviceConfig.channels).
  channels: number[];
  state: DeviceState;
  negotiatedRate?: number;
  negotiatedChannels?: number;
  clientConnected: boolean;
  droppedFrames: number;
  opus?: OpusSettings;
  error?: string;
  friendlyName?: string;
  supportedRates?: number[];
  supportedChannels?: number[];
}

// AvailableDevice is a capture device the host exposes that the configuration
// does not list (GET /devices/available). It carries only the device id and the
// probed capabilities; a name, path and stream parameters are assigned when it
// is provisioned via POST /devices.
export interface AvailableDevice {
  device: string;
  state: "available";
  friendlyName?: string;
  supportedRates?: number[];
  supportedChannels?: number[];
}

// ProvisionDeviceRequest enables a detected device (POST /devices). Only device
// is required; the appliance derives everything else, and any field set here
// overrides its derived default.
export interface ProvisionDeviceRequest {
  device: string;
  name?: string;
  mode?: StreamMode;
  rate?: number;
  // Optional 1-based channel selection; chosen from the device's capabilities
  // when omitted.
  channels?: number[];
}

export interface NetworkInterface {
  name: string;
  mac?: string;
  up: boolean;
  addresses: string[];
  rxBytes: number;
  txBytes: number;
}

export interface SystemInfo {
  platform: string;
  os?: string;
  kernel?: string;
  hostname: string;
  cpuModel?: string;
  cpuCores: number;
  cpuPercent?: number;
  memTotalBytes: number;
  memUsedBytes: number;
  diskTotalBytes: number;
  diskUsedBytes: number;
  tempCelsius?: number;
  network: NetworkInterface[];
}

export interface ChannelLevels {
  channel: number;
  peakDbfs: number;
  rmsDbfs: number;
  clipped: boolean;
}

export interface DeviceLevels {
  name: string;
  channels: ChannelLevels[];
}

export interface LevelsEvent {
  devices: DeviceLevels[];
}

export interface RestartResult {
  status: string;
}

// LoadError is the detail of the store's "loaderror" event. coreFailed marks a
// status+devices failure (the dashboard's data); systemFailed marks a /system
// failure (the system view's data). A view renders its error only for its own
// resource, so one failing endpoint does not blank another view's valid data.
export interface LoadError {
  coreFailed: boolean;
  systemFailed: boolean;
  message: string;
}

export interface Problem {
  type?: string;
  title?: string;
  status?: number;
  detail?: string;
  instance?: string;
}

export interface ValidationErrorItem {
  field?: string;
  reason?: string;
}

export interface ValidationProblem extends Problem {
  errors?: ValidationErrorItem[];
}

// Notification severity ranks an entry for the UI: error raises a toast,
// warning and info are badge-only. Matches NotificationSeverity in openapi.yaml.
export type NotificationSeverity = "error" | "warning" | "info";
// Notification category groups an entry by the subsystem it concerns.
export type NotificationCategory = "device" | "audio" | "stream" | "system" | "config";
// Notification kind distinguishes a one-off event from the onset and clear of a
// condition. A clear pairs with its onset by key.
export type NotificationKind = "event" | "onset" | "clear";

// Notification is one entry in the center (GET /notifications and the
// `notification` SSE event). id is a monotonic per-boot integer; key is present
// on onset and clear entries and absent on discrete events; source names the
// subject (device name, track path, remote address) for a chip.
export interface Notification {
  id: number;
  bootId: string;
  time: string;
  severity: NotificationSeverity;
  category: NotificationCategory;
  kind: NotificationKind;
  key?: string;
  source?: string;
  title: string;
  message: string;
}

// NotificationSnapshot is the full current state a client bootstraps and
// re-syncs from: the boot identity, the server wall clock (for clock-skew
// correction on an RTC-less host), the next id that will be assigned, and every
// ring entry merged with every active condition, ascending by id.
export interface NotificationSnapshot {
  bootId: string;
  serverTime: string;
  nextId: number;
  notifications: Notification[];
}
