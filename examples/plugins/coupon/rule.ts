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
}

export function Create(): Rule {
  return new Rule();
}
