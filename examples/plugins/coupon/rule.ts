// A pricing rule written in TypeScript: a known coupon code takes a fixed amount off, never more than the order's
// subtotal. An unknown code is an error, which the service receives as the call's error.
import { coupons } from "./coupons";

interface Item {
  sku: string;
  quantity: number;
  unitCents: number;
}

interface Order {
  customer: string;
  coupon: string;
  items: Item[] | null;
}

interface Limits {
  maxPercent: number;
  maxCents: number;
}

interface Issue {
  severity: "error" | "warning" | "info";
  code: string;
  message: string;
  field: string;
}

interface Discount {
  cents: number;
  reason: string;
}

class Rule {
  Discount(order: Order): Discount {
    if (order.coupon === "") {
      return { cents: 0, reason: "" };
    }
    if (!Object.hasOwn(coupons, order.coupon)) {
      throw new Error(`unknown coupon ${order.coupon}`);
    }
    const subtotal = (order.items ?? []).reduce((sum, item) => sum + item.quantity * item.unitCents, 0);
    return { cents: Math.min(coupons[order.coupon], subtotal), reason: `coupon ${order.coupon}` };
  }

  // Validate tells plg whether every coupon fits the service's limits (plg.WithValidation): plg calls it in a
  // throwaway instance before the rule goes live, and an "error" issue keeps it from loading.
  Validate(limits: Limits): Issue[] {
    return Object.entries(coupons)
      .filter(([, cents]) => cents > limits.maxCents)
      .map(([code, cents]) => ({
        severity: "error" as const,
        code: "coupon.cents",
        message: `${code} takes ${cents} cents off, over the ${limits.maxCents} cents limit`,
        field: `coupons.${code}`,
      }));
  }
}

export function Create(): Rule {
  return new Rule();
}
