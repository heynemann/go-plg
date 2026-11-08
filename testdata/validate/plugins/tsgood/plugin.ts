// A cohort provider of the users whose loyalty tier is gold.
declare const host: { Tier(user: string): Promise<string> };

class Gold {
  async IsMember(user: string) {
    return (await host.Tier(user)) === "gold";
  }

  List(cursor: string) {
    return { users: [], next: "" };
  }
}

export function Create() {
  return new Gold();
}
