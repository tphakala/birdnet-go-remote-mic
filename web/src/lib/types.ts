/**
 * Core type definitions matching api/openapi.yaml
 */

export type StreamMode = "pcm" | "opus";
export type DeviceState = "serving" | "skipped" | "failed";

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

export interface ConfigPatch {
  listen?: string;
  discovery?: DiscoverySettings;
  management?: ManagementSettings;
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
