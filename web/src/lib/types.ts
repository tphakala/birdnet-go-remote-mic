/**
 * The wire types, taken from api/openapi.yaml through api-schema.gen.ts
 * (`task web:types:generate`; `task web:types:verify` fails when it is stale).
 * This file names them for the UI and keeps what the spec does not say: the
 * UI-only types and the deliberately open unions.
 */
import type { components } from "./api-schema.gen.ts";

type Schemas = components["schemas"];

// Omit that only accepts keys T has, so an override naming a field the spec
// renamed or dropped fails to compile here instead of leaving a stale field.
type Without<T, K extends keyof T> = Omit<T, K>;

export type StreamMode = Schemas["StreamMode"];
export type DeviceState = Schemas["DeviceState"];
export type OpusSettings = Schemas["OpusSettings"];
export type StreamConfig = Schemas["StreamConfig"];
export type DeviceConfig = Schemas["DeviceConfig"];
export type DiscoverySettings = Schemas["DiscoverySettings"];
export type ManagementSettings = Schemas["ManagementSettings"];
export type AuthSettings = Schemas["AuthSettings"];
export type AudioAlertSettings = Schemas["AudioAlertSettings"];
export type HostAlertSettings = Schemas["HostAlertSettings"];
export type NotificationSettings = Schemas["NotificationSettings"];
export type UpdateSettings = Schemas["UpdateSettings"];
export type Config = Schemas["Config"];
export type ConfigPatch = Schemas["ConfigPatch"];
export type ConfigUpdateResult = Schemas["ConfigUpdateResult"];
export type ConfigOverride = Schemas["ConfigOverride"];
export type ApplianceStatus = Schemas["ApplianceStatus"];
export type CertificateInfo = Schemas["CertificateInfo"];
export type CertificateInstallRequest = Schemas["CertificateInstallRequest"];
export type CertificateRegenerateRequest = Schemas["CertificateRegenerateRequest"];
export type AvailableDevice = Schemas["AvailableDevice"];
export type ProvisionDeviceRequest = Schemas["ProvisionDeviceRequest"];
export type NetworkInterface = Schemas["NetworkInterface"];
export type SystemInfo = Schemas["SystemInfo"];
export type UpdateStatus = Schemas["UpdateStatus"];
export type ChannelLevels = Schemas["ChannelLevels"];
export type DeviceLevels = Schemas["DeviceLevels"];
export type LevelsEvent = Schemas["LevelsEvent"];
export type RestartResult = Schemas["RestartResult"];
export type NotificationSeverity = Schemas["NotificationSeverity"];
export type NotificationCategory = Schemas["NotificationCategory"];
export type NotificationKind = Schemas["NotificationKind"];
export type Notification = Schemas["Notification"];
export type NotificationSnapshot = Schemas["NotificationSnapshot"];

// Hand-written overrides. A field the appliance always sends today can be
// absent from an older appliance, and a client must tolerate a later appliance
// adding values, so these are looser than the spec.

// Device.downCause values the API documents. A later appliance may add values,
// so any other string is accepted too (and titled generically).
export type DownCause =
  | Exclude<Schemas["Device"]["downCause"], undefined>
  | (string & {});

// How many clients are playing the stream; absent from an older appliance.
export type StreamStatus = Without<Schemas["StreamStatus"], "clientCount"> & {
  clientCount?: number;
};

// clientCount and overruns are absent from an older appliance.
export type Device = Without<
  Schemas["Device"],
  "downCause" | "clientCount" | "overruns" | "streams"
> & {
  downCause?: DownCause;
  clientCount?: number;
  overruns?: number;
  streams?: StreamStatus[];
};

// authRequired is absent on an older appliance that predates token auth.
export type Health = Without<Schemas["Health"], "authRequired"> & {
  authRequired?: boolean;
};

// The client keeps only the fields a problem body carries, so type is optional
// here although the spec defaults it to about:blank.
export type Problem = Without<Schemas["Problem"], "type"> & { type?: string };

// A validation entry the server malformed is still shown, so both fields are
// optional here.
export interface ValidationErrorItem {
  field?: string;
  reason?: string;
}

export type ValidationProblem = Without<Schemas["ValidationProblem"], "type" | "errors"> & {
  type?: string;
  errors?: ValidationErrorItem[];
};

// The spec's inline enums, named for the UI.
export type InstallMethod = UpdateStatus["installMethod"];
export type UpdatePhase = UpdateStatus["phase"];

// LoadError is the detail of the store's "loaderror" event. coreFailed marks a
// status+devices failure (the dashboard's data, and the appliance is out of
// reach). devicesFailed marks a /devices failure, alone or together with
// /status, which leaves the rack and Stream Status with nothing to show.
// systemFailed marks a /system failure (the system view's data); configFailed
// marks a /config failure (the System view's network/access/notification cards,
// which stay in their loading state until a config arrives). A view surfaces
// the error only for its own resource, so one failing endpoint does not blank
// another view's valid data. availableFailed marks a /devices/available
// failure; it is advisory (a stale unconfigured-hardware list that the next
// poll refreshes) and never triggers the error on its own, but it is carried
// here for completeness so no view has to guess.
export interface LoadError {
  coreFailed: boolean;
  devicesFailed: boolean;
  systemFailed: boolean;
  configFailed: boolean;
  availableFailed: boolean;
  message: string;
}
