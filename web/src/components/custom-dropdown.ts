let dropdownSeq = 0;

export class CustomDropdown {
  private container: HTMLElement;
  private trigger: HTMLElement;
  private menu: HTMLElement;
  private items: HTMLElement[];
  private hiddenInput: HTMLInputElement | null;
  private onChange?: (val: string) => void;
  private docClickHandler!: (e: MouseEvent) => void;
  private activeIndex = -1;

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
    const menuEl = this.container.querySelector<HTMLElement>(".dropdown-menu");
    if (!menuEl) throw new Error("Dropdown menu element not found");
    this.menu = menuEl;
    this.hiddenInput = this.container.querySelector<HTMLInputElement>("input[type='hidden']");
    this.onChange = onChange;
    this.items = Array.from(this.container.querySelectorAll<HTMLElement>(".dropdown-item"));

    this.wireAria();
    this.bindEvents();
  }

  // wireAria gives the menu and options stable ids and connects the combobox
  // trigger to them so a screen reader announces the active option. Idempotent
  // for dropdowns whose markup already carries these attributes.
  private wireAria(): void {
    const uid = ++dropdownSeq;
    if (!this.menu.id) this.menu.id = `dropdown-menu-${uid}`;
    this.trigger.setAttribute("role", "combobox");
    this.trigger.setAttribute("aria-haspopup", "listbox");
    this.trigger.setAttribute("aria-controls", this.menu.id);
    this.menu.setAttribute("role", "listbox");
    this.items.forEach((item, i) => {
      if (!item.id) item.id = `${this.menu.id}-opt-${i}`;
      item.setAttribute("role", "option");
    });
  }

  private bindEvents(): void {
    this.trigger.addEventListener("click", (e) => {
      e.stopPropagation();
      this.toggle();
    });

    this.trigger.addEventListener("keydown", (e) => this.onKeydown(e));

    this.items.forEach((item) => {
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

  private onKeydown(e: KeyboardEvent): void {
    const open = this.container.classList.contains("open");
    switch (e.key) {
      case "Enter":
      case " ":
        e.preventDefault();
        if (open && this.activeIndex >= 0) {
          const val = this.items[this.activeIndex]?.dataset.val;
          if (val !== undefined) this.select(val);
        } else {
          this.toggle();
        }
        break;
      case "Escape":
        if (open) {
          e.preventDefault();
          this.close();
        }
        break;
      case "ArrowDown":
        e.preventDefault();
        if (!open) this.open();
        else this.setActive(this.activeIndex + 1);
        break;
      case "ArrowUp":
        e.preventDefault();
        if (!open) this.open();
        else this.setActive(this.activeIndex - 1);
        break;
      case "Home":
        if (open) {
          e.preventDefault();
          this.setActive(0);
        }
        break;
      case "End":
        if (open) {
          e.preventDefault();
          this.setActive(this.items.length - 1);
        }
        break;
      case "Tab":
        if (open) this.close();
        break;
    }
  }

  // setActive moves the roving highlight to index (clamped), updates
  // aria-activedescendant, and scrolls the option into view.
  private setActive(index: number): void {
    if (this.items.length === 0) return;
    const i = Math.max(0, Math.min(index, this.items.length - 1));
    this.items.forEach((item, n) => item.classList.toggle("active", n === i));
    this.activeIndex = i;
    const active = this.items[i];
    this.trigger.setAttribute("aria-activedescendant", active.id);
    active.scrollIntoView({ block: "nearest" });
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
        const trigger = el.querySelector(".dropdown-trigger");
        trigger?.setAttribute("aria-expanded", "false");
        // Clear the other dropdown's roving-highlight state too, so it does not
        // leave aria-activedescendant pointing at a now-hidden option.
        trigger?.removeAttribute("aria-activedescendant");
        el.querySelectorAll(".dropdown-item.active").forEach((item) => item.classList.remove("active"));
      }
    });
    this.container.classList.add("open");
    this.trigger.setAttribute("aria-expanded", "true");
    // Start the roving highlight on the selected option.
    const selected = this.items.findIndex((item) => item.classList.contains("selected"));
    this.setActive(selected >= 0 ? selected : 0);
  }

  public close(): void {
    this.container.classList.remove("open");
    this.trigger.setAttribute("aria-expanded", "false");
    this.trigger.removeAttribute("aria-activedescendant");
    this.items.forEach((item) => item.classList.remove("active"));
    this.activeIndex = -1;
  }

  public select(val: string): void {
    let selectedItem: HTMLElement | null = null;

    this.items.forEach((item) => {
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
