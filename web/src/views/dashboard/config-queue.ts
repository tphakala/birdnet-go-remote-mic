import { showToast } from "../../components/toast.ts";
import { apiErrorMessage, firstProblem } from "../../lib/api.ts";
import { deviceToConfig, rejectionText } from "../../lib/dashboard-core.ts";
import { store } from "../../lib/store.ts";
import type { Config, DeviceConfig } from "../../lib/types.ts";

// STALE_BASE_TEXT is said when a change must build on the appliance's config
// and a re-read of it failed (see ConfigQueue.freshBase).
export const STALE_BASE_TEXT = "Could not read the current configuration; nothing was changed. Try again in a moment.";

// ConfigQueue serializes the Dashboard's device mutations (an enable toggle, a
// settings save, a Remove, an Enable), so each full-array PATCH is built from a
// fresh base only after the previous mutation settled: a PATCH from a stale
// base would clobber a concurrent change. It also tracks whether the cached
// config may differ from the appliance's. One queue serves every card.
export class ConfigQueue {
  private tail: Promise<void> = Promise.resolve();
  // Set when the cached config may not match the appliance: a change's
  // outcome was unknown and its re-read failed. The next full-array PATCH
  // re-reads first (freshBase).
  private baseStale = false;

  // enqueue runs task once the previous mutation's PATCH and refresh have
  // settled, so it can build its PATCH from a fresh base(). The chain tail
  // swallows a task's rejection (the caller still sees it), so one failure
  // cannot wedge later mutations.
  public enqueue(task: () => Promise<void>): Promise<void> {
    const run = this.tail.then(() => task());
    this.tail = run.catch(() => {});
    return run;
  }

  // base is the current device list to patch from: the persisted config when
  // loaded, else the runtime devices projected to config shape so a patch
  // built before the first config load still carries every device.
  public base(): DeviceConfig[] {
    return store.getState().config?.devices ?? store.getState().devices.map(deviceToConfig);
  }

  // freshBase makes sure a full-array PATCH builds from the appliance's
  // config: after a change whose outcome is unknown and whose re-read failed
  // (baseStale), it reads the config first, and reports false if it cannot.
  public async freshBase(): Promise<boolean> {
    if (!this.baseStale) return true;
    this.baseStale = !(await store.refreshConfig());
    return !this.baseStale;
  }

  // applied seeds the cached config with a PATCH's authoritative response,
  // before any refresh, so a later queued mutation rebuilds its base from this
  // change even if the refresh fails.
  public applied(config: Config): void {
    store.applyConfig(config);
    this.baseStale = false;
  }

  // refreshDeviceViews re-reads what a device change affects (devices,
  // available, config) and reports whether the device list was read, which
  // alone decides whether a device is listed, and whether all three were. A
  // failed config read marks the cached base stale.
  public async refreshDeviceViews(): Promise<{ devices: boolean; all: boolean }> {
    const [devices, available, config] = await Promise.all([store.refreshDevices(), store.refreshAvailable(), store.refreshConfig()]);
    this.baseStale = !config;
    return { devices, all: devices && available && config };
  }

  // refreshConfigViews re-reads config and devices after a config PATCH, as
  // refreshDeviceViews does.
  public async refreshConfigViews(): Promise<boolean> {
    const [config, devices] = await Promise.all([store.refreshConfig(), store.refreshDevices()]);
    this.baseStale = !config;
    return config && devices;
  }
}

// apiErrorToast surfaces a failed request under a prefix that says which
// action failed. A validation problem names the first field by its form
// label, and the device by its name in sent, the device list the request
// carried (the stored config when the request carried none), since a field
// path indexes that list.
export function apiErrorToast(err: unknown, prefix: string, sent?: readonly DeviceConfig[]): void {
  const problem = firstProblem(err);
  if (problem) {
    const names = (sent ?? store.getState().config?.devices ?? []).map((cd) => cd.name);
    showToast(rejectionText(prefix, problem, names), "error");
  } else {
    showToast(`${prefix}: ${apiErrorMessage(err)}`, "error");
  }
}
