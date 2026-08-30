export class CustomDropdown {
  private container: HTMLElement;
  private trigger: HTMLElement;
  private hiddenInput: HTMLInputElement | null;
  private onChange?: (val: string) => void;
  private docClickHandler!: (e: MouseEvent) => void;

  constructor(containerIdOrEl: string | HTMLElement, onChange?: (val: string) => void) {
    if (typeof containerIdOrEl === "string") {
      const el = document.getElementById(containerIdOrEl);
      if (!el) throw new Error(`Dropdown #${containerIdOrEl} not found`);
      this.container = el;
    } else {
      this.container = containerIdOrEl;
    }

    const triggerEl = this.container.querySelector<HTMLElement>(".dropdown-trigger");
    if (!triggerEl) throw new Error("Dropdown trigger element not found");
    this.trigger = triggerEl;
    this.hiddenInput = this.container.querySelector<HTMLInputElement>("input[type='hidden']");
    this.onChange = onChange;

    this.bindEvents();
  }

  private bindEvents(): void {
    this.trigger.addEventListener("click", (e) => {
      e.stopPropagation();
      this.toggle();
    });

    this.trigger.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        this.toggle();
      } else if (e.key === "Escape") {
        this.close();
      }
    });

    this.container.querySelectorAll<HTMLElement>(".dropdown-item").forEach((item) => {
      item.addEventListener("click", (e) => {
        e.stopPropagation();
        const val = item.dataset.val;
        if (val !== undefined) {
          this.select(val);
        }
      });
    });

    this.docClickHandler = (e: MouseEvent) => {
      if (!this.container.contains(e.target as Node)) {
        this.close();
      }
    };
    document.addEventListener("click", this.docClickHandler);
  }

  public destroy(): void {
    document.removeEventListener("click", this.docClickHandler);
  }

  public toggle(): void {
    if (this.container.classList.contains("open")) {
      this.close();
    } else {
      this.open();
    }
  }

  public open(): void {
    // Close other dropdowns
    document.querySelectorAll(".custom-dropdown.open").forEach((el) => {
      if (el !== this.container) {
        el.classList.remove("open");
        el.querySelector(".dropdown-trigger")?.setAttribute("aria-expanded", "false");
      }
    });
    this.container.classList.add("open");
    this.trigger.setAttribute("aria-expanded", "true");
  }

  public close(): void {
    this.container.classList.remove("open");
    this.trigger.setAttribute("aria-expanded", "false");
  }

  public select(val: string): void {
    const items = this.container.querySelectorAll<HTMLElement>(".dropdown-item");
    let selectedItem: HTMLElement | null = null;

    items.forEach((item) => {
      if (item.dataset.val === val) {
        item.classList.add("selected");
        item.setAttribute("aria-selected", "true");
        selectedItem = item;
      } else {
        item.classList.remove("selected");
        item.setAttribute("aria-selected", "false");
      }
    });

    if (selectedItem) {
      const itemEl = selectedItem as HTMLElement;
      this.container.dataset.value = val;
      if (this.hiddenInput) {
        this.hiddenInput.value = val;
        this.hiddenInput.dispatchEvent(new Event("change", { bubbles: true }));
      }

      // Update trigger visual group
      const valGroup = this.trigger.querySelector(".dropdown-value-group");
      if (valGroup) {
        const itemTag = itemEl.dataset.tag;
        const tagClass = itemEl.dataset.tagClass || "highlight";
        const labelText = itemEl.dataset.label || itemEl.querySelector(".item-title span")?.textContent || val;

        // Build with DOM nodes so a label/tag that ever comes from API or user
        // data cannot inject markup.
        valGroup.textContent = "";
        if (itemTag) {
          const tagEl = document.createElement("span");
          tagEl.className = `tech-tag ${tagClass}`;
          tagEl.style.fontSize = "10px";
          tagEl.style.padding = "1px 5px";
          tagEl.textContent = itemTag;
          valGroup.appendChild(tagEl);
        }
        const labelEl = document.createElement("span");
        labelEl.className = "dropdown-selected-label";
        labelEl.textContent = labelText;
        valGroup.appendChild(labelEl);
      }
    }

    this.close();
    if (this.onChange) {
      this.onChange(val);
    }
  }
}
