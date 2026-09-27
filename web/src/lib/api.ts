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
  ValidationErrorItem,
  ValidationProblem,
} from "./types.ts";

export class ApiError extends Error {
  public status: number;
  public title: string;
  public detail?: string;
  public errors?: ValidationErrorItem[];
  // problem is true when the error came from an RFC 9457 problem body
  // (application/problem+json, what the appliance sends), whose detail is
  // written for people; otherwise detail holds whatever the response body was
  // (a proxy's HTML or JSON error page, say). Read it through problemDetail.
  public problem: boolean;

  constructor(status: number, title: string, detail?: string, errors?: ValidationErrorItem[], problem = false) {
    super(detail || title);
    this.name = "ApiError";
    this.status = status;
    this.title = title;
    this.detail = detail;
    this.errors = errors;
    this.problem = problem;
  }

  // problemDetail is the problem's detail, or undefined when the body was not
  // a problem, so no caller shows a body the appliance did not write.
  get problemDetail(): string | undefined {
    return this.problem ? this.detail : undefined;
  }
}

// apiErrorMessage reduces any thrown value to a short human string. An
// ApiError shows its problem detail, which says what went wrong (problem
// titles are generic: "bad request", "internal error"), else its title, else
// its status; a 401 says the token was not accepted, since the login prompt
// opens with it and the problem's detail names an HTTP header. Any other
// Error shows its message, and anything else its string form. Shared so
// every failure toast maps errors the same way.
export function apiErrorMessage(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 401) return "The access token was not accepted.";
    return err.problemDetail || err.title || `HTTP ${err.status}`;
  }
  if (err instanceof Error) return err.message;
  return String(err);
}

// FieldProblem is one validation problem from a rejected request: the field
// it names, if any, and why it was refused.
export type FieldProblem = ValidationErrorItem & { reason: string };

// problemReason is why a validation item was refused: its own reason, else
// the problem title. Every site that shows a validation problem uses it, so
// they all fall back the same way.
export function problemReason(err: ApiError, item: ValidationErrorItem): string {
  return item.reason ?? err.title;
}

// problemFor is the first validation problem an ApiError carries that match
// accepts, with its reason (problemReason), or null for any other failure.
export function problemFor(err: unknown, match: (item: ValidationErrorItem) => boolean): FieldProblem | null {
  if (!(err instanceof ApiError)) return null;
  const item = err.errors?.find(match);
  if (!item) return null;
  return { field: item.field, reason: problemReason(err, item) };
}

// firstProblem is the first validation problem an ApiError carries, or null
// for any other failure.
export function firstProblem(err: unknown): FieldProblem | null {
  return problemFor(err, () => true);
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
        // Only a problem body's fields are the appliance's words; a plain JSON
        // error (a proxy's) keeps nothing but its status.
        const isProblem = contentType.includes("application/problem+json");
        throw new ApiError(
          (isProblem && prob.status) || res.status,
          (isProblem && prob.title) || res.statusText || `HTTP ${res.status}`,
          isProblem && typeof prob.detail === "string" ? prob.detail : undefined,
          isProblem && Array.isArray(prob.errors) ? prob.errors : undefined,
          isProblem,
        );
      }
      const text = await res.text();
      // statusText is empty over HTTP/2, so fall back to the status code.
      throw new ApiError(res.status, res.statusText || `HTTP ${res.status}`, text);
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
