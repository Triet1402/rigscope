/** Validation errors that map to HTTP 400 without leaking input values. */
export class AlertValidationError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "AlertValidationError";
  }
}

export class AlertNotFoundError extends Error {
  constructor(message = "alert rule not found") {
    super(message);
    this.name = "AlertNotFoundError";
  }
}
