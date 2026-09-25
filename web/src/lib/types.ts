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
  // Whether the device raises the very-quiet audio condition; defaults to true
  // when absent. Set false for a device expected to be silent for long stretches
  // (a bat microphone by day). Stuck-at-zero and clipping are unaffected.
  quietAlert?: boolean;
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

// AudioAlertSettings holds the audio-signal condition thresholds. In a read
// every field is materialized; in a patch an absent field leaves it unchanged.
export interface AudioAlertSettings {
  quietDbfs?: number;
  quietSeconds?: number;
  zeroSeconds?: number;
  clipPercent?: number;
  clipWindowSeconds?: number;
}

// HostAlertSettings holds the host-health condition thresholds. Each clear
// threshold must sit below its onset to keep a hysteresis gap.
export interface HostAlertSettings {
  cpuPercent?: number;
  cpuClearPercent?: number;
  tempCelsius?: number;
  tempClearCelsius?: number;
  diskPercent?: number;
  diskClearPercent?: number;
  memFreePercent?: number;
  memFreeMiB?: number;
}

// NotificationSettings configures the condition monitors behind the notification
// center. In a read every field is materialized; in a patch an absent field (or
// an absent nested object) leaves the current value unchanged, so a partial
// block like { host: { cpuPercent: 95 } } touches only that field.
export interface NotificationSettings {
  enabled?: boolean;
  audio?: AudioAlertSettings;
  host?: HostAlertSettings;
}

export interface Config {
  listen: string;
  discovery?: DiscoverySettings;
  management?: ManagementSettings;
  auth?: AuthSettings;
  notifications?: NotificationSettings;
  devices: DeviceConfig[];
}

// Only discovery, auth, notifications and devices are patchable; the server
// ignores anything else (see api/openapi.yaml ConfigPatch).
export interface ConfigPatch {
  discovery?: DiscoverySettings;
  auth?: AuthSettings;
  notifications?: NotificationSettings;
  devices?: DeviceConfig[];
}

export interface ConfigUpdateResult {
  config: Config;
  restartRequired: boolean;
}

// Health is the open /healthz response: liveness, version, and whether the
// appliance requires an access token. The boot sequence reads authRequired to
// decide whether to settle access before firing gated requests. authRequired is
// absent on an older appliance that predates token auth.
export interface Health {
  status: string;
  version: string;
  authRequired?: boolean;
}

// ConfigOverride names one config field a serve CLI flag overrode for this run:
// the value in force now (effective) versus what the config file holds
// (persisted). Present only when the two differ.
export interface ConfigOverride {
  field: string;
  effective: string;
  persisted: string;
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
  // Config fields a serve CLI flag overrode for this run; absent or empty when
  // none are active.
  overrides?: ConfigOverride[];
}

// CertificateInfo is the public metadata of the management listener's TLS
// certificate (GET /system/certificate, and the body of a successful PUT or
// regenerate). It never carries private key material.
export interface CertificateInfo {
  subject: string;
  issuer: string;
  selfSigned: boolean;
  // true: the appliance minted this self-signed certificate and manages it
  // (regenerates it itself). false: an operator-installed custom certificate,
  // which the appliance never replaces on its own.
  managed: boolean;
  dnsNames: string[];
  ipAddresses: string[];
  notBefore: string;
  notAfter: string;
  fingerprintSha256: string;
}

// CertificateInstallRequest is the body of PUT /system/certificate: an
// operator-supplied certificate and its private key, both PEM. A 422 names the
// offending field as certPem or keyPem.
export interface CertificateInstallRequest {
  certPem: string;
  keyPem: string;
}

// CertificateRegenerateRequest is the body of POST /system/certificate/regenerate.
// The body itself is required by the contract even when there are no extras, so
// the client always sends at least {}. A 422 names an entry as extraSans[i].
export interface CertificateRegenerateRequest {
  extraSans?: string[];
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
  // Every channel any of the device's streams carries (channels covers only the
  // first stream). Absent from an older appliance.
  streamedChannels?: number[];
  state: DeviceState;
  negotiatedRate?: number;
  negotiatedChannels?: number;
  // Hardware capture format token the device negotiated (s16, s24_le, s24_3le,
  // s32). Absent unless the device opened, or from an older appliance. A wider
  // capture is downconverted to the S16LE stream.
  negotiatedFormat?: string;
  clientConnected: boolean;
  droppedFrames: number;
  opus?: OpusSettings;
  error?: string;
  // Class of why a skipped or failed device is not serving (not-connected,
  // ambiguous, malformed, resolve-failed, same-hardware, open-failed,
  // disconnected, failed). Absent while serving, from an older appliance, and
  // for a skip with no specific class; a later appliance may add values.
  downCause?: string;
  friendlyName?: string;
  // Current-boot ALSA address ("hw:4,0") the configured id resolved to, for
  // display only; absent when the id resolved to no single present device (not
  // connected, ambiguous, a resolve failure, or a card-index device opened
  // without a resolution).
  hwAddr?: string;
  // False when the configured id names a card by its kernel index, which can
  // point at a different device after a reboot or replug.
  idStable?: boolean;
  supportedRates?: number[];
  supportedChannels?: number[];
}

// AvailableDevice is a capture device the host exposes that the configuration
// does not list (GET /devices/available). It carries only the device id and the
// probed capabilities; a name, path and stream parameters are assigned when it
// is provisioned via POST /devices.
export interface AvailableDevice {
  // The stable id provisioning persists (see idStable).
  device: string;
  state: "available";
  // Current-boot ALSA address, for display only.
  hwAddr?: string;
  // False when the host offered no stable id, so device is a card index.
  idStable?: boolean;
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
// failure (the system view's data); configFailed marks a /config failure (the
// System view's network/access/notification cards, which stay hidden until a
// config arrives). A view surfaces the error only for its own resource, so one
// failing endpoint does not blank another view's valid data. availableFailed marks
// a /devices/available failure; it is advisory (a stale unconfigured-hardware list
// that the next poll refreshes) and never triggers the error on its own, but it is
// carried here for completeness so no view has to guess.
export interface LoadError {
  coreFailed: boolean;
  systemFailed: boolean;
  configFailed: boolean;
  availableFailed: boolean;
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
// subject (device name, track path, remote address) for a chip. uptimeMs is the
// notification center's monotonic uptime at publish; unlike the wall-clock time, a
// server clock step does not move it.
export interface Notification {
  id: number;
  bootId: string;
  time: string;
  uptimeMs: number;
  severity: NotificationSeverity;
  category: NotificationCategory;
  kind: NotificationKind;
  key?: string;
  source?: string;
  title: string;
  message: string;
}

// NotificationSnapshot is the full current state a client bootstraps and
// re-syncs from: the boot identity, the server wall clock and monotonic uptime
// read at one instant (the client pairs uptimeMs with its own clock on receipt
// to place entries; serverTime is informational), the ring depth, the next id
// that will be assigned, and every ring entry merged with every active
// condition, ascending by id.
export interface NotificationSnapshot {
  bootId: string;
  serverTime: string;
  uptimeMs: number;
  capacity: number;
  nextId: number;
  notifications: Notification[];
}
