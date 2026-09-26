import type {
  ApplianceStatus,
  AvailableDevice,
  CertificateInfo,
  CertificateInstallRequest,
  CertificateRegenerateRequest,
  Config,
  ConfigPatch,
  ConfigUpdateResult,
  Device,
  Health,
  NotificationSnapshot,
  ProvisionDeviceRequest,
  RestartResult,
  SystemInfo,
  UpdateStatus,
  ValidationProblem,
} from "./types.js";

export class ApiError extends Error {
  constructor(
    public status: number,
    public title: string,
    public detail?: string,
    public errors?: Array<{ field?: string; reason?: string }>
  ) {
    super(detail || title);
    this.name = "ApiError";
  }
}

export class ApiClient {
  private baseUrl: string;
  private token: string | null = null;
  // onUnauthorized fires on a 401. The `used === this.token` guard below only
  // suppresses a 401 whose RESPONSE is processed after setToken already changed
  // the token (a request sent under the old token, resolving after the swap).
  // It cannot suppress a 401 processed while both `used` and `this.token` still
  // hold the pre-rotation token, which happens because the appliance enforces
  // the new token before it finishes writing the PATCH response. The store owns
  // a swap window (beginTokenSwap/endTokenSwap) that covers that remaining gap.
  public onUnauthorized: (() => void) | null = null;

  constructor(baseUrl: string = "/api/v1") {
    this.baseUrl = baseUrl;
  }

  public setToken(token: string | null): void {
    this.token = token;
  }

  private async request<T>(path: string, options: RequestInit = {}): Promise<T> {
    const headers = new Headers(options.headers || {});
    headers.set("Accept", "application/json, application/problem+json");

    const used = this.token;
    if (used) {
      headers.set("Authorization", `Bearer ${used}`);
    }

    if (options.body && !headers.has("Content-Type")) {
      headers.set("Content-Type", "application/json");
    }

    const res = await fetch(`${this.baseUrl}${path}`, {
      ...options,
      headers,
    });

    if (res.status === 401 && used === this.token) {
      this.onUnauthorized?.();
    }

    if (res.status === 204) {
      return {} as T;
    }

    const contentType = res.headers.get("Content-Type") || "";
    const isJson = contentType.includes("application/json") || contentType.includes("application/problem+json");

    if (!res.ok) {
      if (isJson) {
        const prob = (await res.json()) as ValidationProblem;
        throw new ApiError(
          prob.status || res.status,
          prob.title || res.statusText,
          prob.detail,
          prob.errors
        );
      }
      const text = await res.text();
      throw new ApiError(res.status, res.statusText, text);
    }

    if (isJson) {
      return (await res.json()) as T;
    }

    return (await res.text()) as unknown as T;
  }

  public async getHealth(): Promise<Health> {
    return this.request<Health>("/healthz");
  }

  public async getStatus(): Promise<ApplianceStatus> {
    return this.request<ApplianceStatus>("/status");
  }

  public async getDevices(): Promise<Device[]> {
    return this.request<Device[]>("/devices");
  }

  public async getDevice(name: string): Promise<Device> {
    return this.request<Device>(`/devices/${encodeURIComponent(name)}`);
  }

  public async getAvailableDevices(): Promise<AvailableDevice[]> {
    return this.request<AvailableDevice[]>("/devices/available");
  }

  public async provisionDevice(req: ProvisionDeviceRequest): Promise<Device> {
    return this.request<Device>("/devices", {
      method: "POST",
      body: JSON.stringify(req),
    });
  }

  public async deleteDevice(name: string): Promise<void> {
    await this.request<void>(`/devices/${encodeURIComponent(name)}`, {
      method: "DELETE",
    });
  }

  public async getConfig(): Promise<Config> {
    return this.request<Config>("/config");
  }

  public async patchConfig(patch: ConfigPatch): Promise<ConfigUpdateResult> {
    return this.request<ConfigUpdateResult>("/config", {
      method: "PATCH",
      body: JSON.stringify(patch),
    });
  }

  public async getSystem(): Promise<SystemInfo> {
    return this.request<SystemInfo>("/system");
  }

  public async getCertificate(): Promise<CertificateInfo> {
    return this.request<CertificateInfo>("/system/certificate");
  }

  // getCertificatePem returns the PEM-encoded public certificate as text. The
  // response is not JSON, so request() returns its body verbatim; the bearer
  // token is still attached, which a bare link navigation could not do.
  public async getCertificatePem(): Promise<string> {
    return this.request<string>("/system/certificate/pem");
  }

  // installCertificate replaces the management certificate with an operator-
  // supplied certificate and key. The appliance applies it live and returns the
  // new metadata; a 422 carries per-field errors (certPem, keyPem).
  public async installCertificate(req: CertificateInstallRequest): Promise<CertificateInfo> {
    return this.request<CertificateInfo>("/system/certificate", {
      method: "PUT",
      body: JSON.stringify(req),
    });
  }

  // regenerateCertificate asks the appliance to mint a fresh self-signed
  // certificate, optionally with extra SANs, and apply it live. The contract
  // requires a JSON body, so the default {} guarantees one is always sent.
  public async regenerateCertificate(req: CertificateRegenerateRequest = {}): Promise<CertificateInfo> {
    return this.request<CertificateInfo>("/system/certificate/regenerate", {
      method: "POST",
      body: JSON.stringify(req),
    });
  }

  public async getNotifications(): Promise<NotificationSnapshot> {
    return this.request<NotificationSnapshot>("/notifications");
  }

  public async postSystemRestart(): Promise<RestartResult> {
    return this.request<RestartResult>("/system/restart", {
      method: "POST",
    });
  }

  // checkForUpdate checks for a newer release now; a failed check is not an
  // error response but a status carrying lastError. A 409 means checks are
  // off or the build is not a release.
  public async checkForUpdate(): Promise<UpdateStatus> {
    return this.request<UpdateStatus>("/system/update/check", {
      method: "POST",
    });
  }

  // startUpdate starts the one-button update to the newest release found. It
  // returns at once in the downloading phase; the appliance restarts when the
  // root updater installs it. A 409 means there is nothing to install, this
  // installation cannot update itself, update checks are off, or an update is
  // already running.
  public async startUpdate(): Promise<UpdateStatus> {
    return this.request<UpdateStatus>("/system/update", {
      method: "POST",
    });
  }
}

export const api = new ApiClient();
