// A cohort provider of the users holding a coupon, by the repository's shared lib.
import { couponTier } from "./lib/tiers";

declare const host: { Tier(user: string): Promise<string> };

export function Create() {
  if (couponTier() !== "coupon") {
    throw new Error("the shared lib does not tell coupon");
  }
  return {
    async IsMember(user: string) {
      return (await host.Tier(user)) === couponTier();
    },
    List(cursor: string) {
      return { users: [], next: "" };
    },
  };
}
