import type {
  ApplianceStatus,
  AvailableDevice,
  Config,
  ConfigPatch,
  ConfigUpdateResult,
  Device,
  ProvisionDeviceRequest,
  RestartResult,
  SystemInfo,
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

  constructor(baseUrl: string = "/api/v1") {
    this.baseUrl = baseUrl;
  }

  public setToken(token: string | null): void {
    this.token = token;
  }

  private async request<T>(path: string, options: RequestInit = {}): Promise<T> {
    const headers = new Headers(options.headers || {});
    headers.set("Accept", "application/json, application/problem+json");

    if (this.token) {
      headers.set("Authorization", `Bearer ${this.token}`);
    }

    if (options.body && !headers.has("Content-Type")) {
      headers.set("Content-Type", "application/json");
    }

    const res = await fetch(`${this.baseUrl}${path}`, {
      ...options,
      headers,
    });

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

  public async getHealth(): Promise<{ status: string; version: string }> {
    return this.request<{ status: string; version: string }>("/healthz");
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

  public async postSystemRestart(): Promise<RestartResult> {
    return this.request<RestartResult>("/system/restart", {
      method: "POST",
    });
  }
}

export const api = new ApiClient();
