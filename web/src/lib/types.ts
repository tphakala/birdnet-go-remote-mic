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
