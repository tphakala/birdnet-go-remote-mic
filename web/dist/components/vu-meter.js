export class VUMeter {
    canvas;
    ctx;
    peakValEl;
    clipEl;
    rmsVal = -60;
    peakVal = -60;
    peakHoldVal = -60;
    peakHoldTimer = 0;
    isClipped = false;
    animFrameId = null;
    lastTime = performance.now();
    // When the viewer prefers reduced motion, skip the free-running rAF loop and
    // the peak-needle decay animation, redrawing a static bar on each level
    // update instead.
    reducedMotion;
    constructor(canvas, peakValEl, clipEl) {
        this.canvas = canvas;
        const context = this.canvas.getContext("2d");
        if (!context) {
            throw new Error("Canvas 2D context is not available");
        }
        this.ctx = context;
        this.peakValEl = peakValEl ?? null;
        this.clipEl = clipEl ?? null;
        this.reducedMotion = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches ?? false;
        if (this.clipEl) {
            this.clipEl.addEventListener("click", () => this.clearClip());
        }
        if (this.reducedMotion) {
            this.render();
        }
        else {
            this.startLoop();
        }
    }
    setLevels(rms, peak, clipped = false) {
        this.rmsVal = isFinite(rms) ? rms : -60;
        this.peakVal = isFinite(peak) ? peak : -60;
        if (clipped || this.peakVal >= -0.1) {
            this.isClipped = true;
        }
        if (this.peakVal > this.peakHoldVal) {
            this.peakHoldVal = this.peakVal;
            this.peakHoldTimer = 45; // Hold peak needle for ~45 frames before decay
        }
        // With no animation loop running, reflect the new levels immediately and
        // pin the peak to the current value (no animated decay).
        if (this.reducedMotion) {
            this.peakHoldVal = this.peakVal;
            this.render();
        }
    }
    clearClip() {
        this.isClipped = false;
        if (this.clipEl) {
            this.clipEl.classList.remove("clipped");
        }
    }
    startLoop() {
        const loop = (now) => {
            const dt = Math.min((now - this.lastTime) / 1000, 0.1);
            this.lastTime = now;
            // Peak needle decay
            if (this.peakHoldTimer > 0) {
                this.peakHoldTimer -= 1;
            }
            else {
                this.peakHoldVal = Math.max(-60, this.peakHoldVal - 30 * dt);
            }
            this.render();
            this.animFrameId = requestAnimationFrame(loop);
        };
        this.animFrameId = requestAnimationFrame(loop);
    }
    destroy() {
        if (this.animFrameId !== null) {
            cancelAnimationFrame(this.animFrameId);
            this.animFrameId = null;
        }
    }
    dbToRatio(db) {
        // Calibrate -60 dBFS to 0.0 and 0 dBFS to 1.0
        if (db <= -60)
            return 0;
        if (db >= 0)
            return 1;
        return (db + 60) / 60;
    }
    render() {
        const w = this.canvas.width;
        const h = this.canvas.height;
        const ctx = this.ctx;
        ctx.clearRect(0, 0, w, h);
        // Background track
        ctx.fillStyle = "rgba(255, 255, 255, 0.04)";
        ctx.fillRect(0, 0, w, h);
        // Segmented meter settings
        const numSegments = 36;
        const gap = 2;
        const segWidth = (w - (numSegments - 1) * gap) / numSegments;
        const rmsRatio = this.dbToRatio(this.rmsVal);
        const activeSegments = Math.round(rmsRatio * numSegments);
        for (let i = 0; i < numSegments; i++) {
            const x = i * (segWidth + gap);
            const segRatio = i / numSegments;
            const segDb = -60 + segRatio * 60;
            let color = "rgba(16, 185, 129, 0.85)"; // Green
            if (segDb > -12 && segDb <= -3) {
                color = "rgba(245, 158, 11, 0.9)"; // Amber
            }
            else if (segDb > -3) {
                color = "rgba(239, 68, 68, 0.95)"; // Red
            }
            if (i < activeSegments) {
                ctx.fillStyle = color;
            }
            else {
                ctx.fillStyle = "rgba(255, 255, 255, 0.06)";
            }
            ctx.fillRect(x, 0, segWidth, h);
        }
        // Peak hold needle
        const peakHoldRatio = this.dbToRatio(this.peakHoldVal);
        if (peakHoldRatio > 0.02) {
            const peakX = Math.min(w - 2, Math.max(0, peakHoldRatio * w - 1.5));
            let needleColor = "#10b981";
            if (this.peakHoldVal > -12 && this.peakHoldVal <= -3) {
                needleColor = "#f59e0b";
            }
            else if (this.peakHoldVal > -3) {
                needleColor = "#ef4444";
            }
            ctx.fillStyle = needleColor;
            ctx.fillRect(peakX, 0, 2, h);
        }
        // Update DOM indicators
        if (this.peakValEl) {
            const formatted = this.peakHoldVal <= -59.9 ? "-inf" : `${this.peakHoldVal.toFixed(1)} dBFS`;
            this.peakValEl.textContent = formatted;
        }
        if (this.clipEl) {
            if (this.isClipped) {
                this.clipEl.classList.add("clipped");
            }
            else {
                this.clipEl.classList.remove("clipped");
            }
        }
    }
}
