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
  channels: number;
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

export interface Config {
  listen: string;
  discovery?: DiscoverySettings;
  management?: ManagementSettings;
  devices: DeviceConfig[];
}

// Only discovery and devices are patchable; the server ignores anything else
// (see api/openapi.yaml ConfigPatch).
export interface ConfigPatch {
  discovery?: DiscoverySettings;
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
  channels: number;
  state: DeviceState;
  negotiatedRate?: number;
  negotiatedChannels?: number;
  clientConnected: boolean;
  droppedFrames: number;
  opus?: OpusSettings;
  error?: string;
  friendlyName?: string;
  supportedRates?: number[];
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

export interface DeviceLevels {
  name: string;
  peakDbfs: number;
  rmsDbfs: number;
  clipped: boolean;
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
