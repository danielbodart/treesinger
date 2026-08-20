/**
 * A clock is the single source of "now" for every time-driven decision
 * (token expiry, refresh skew, profile-cache TTL). Injecting it keeps those
 * decisions pure functions of `clock.now()` + stored state, so tests advance a
 * StoppedClock instead of waiting on real time.
 */
export interface Clock {
    now(): Date;
}

export class SystemClock implements Clock {
    now(): Date {
        return new Date();
    }
}

export class StoppedClock implements Clock {
    constructor(private t: Date) {}

    now(): Date {
        return this.t;
    }

    advance(ms: number): void {
        this.t = new Date(this.t.getTime() + ms);
    }
}
