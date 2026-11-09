// IsMember answers the user's tier instead of whether the user is a member, so what Create returns is no
// cohort.Provider; nothing in the plugin says it should be one, so the type-check of Create's result reports it.
class Tiers {
  async IsMember(user: string) {
    return await host.Tier(user);
  }

  List(cursor: string) {
    return { users: [], next: "" };
  }
}

export function Create() {
  return new Tiers();
}
