// The TypeScript test plugin that calls back into its host: it implements testcontract.Hosted. Host methods
// return Promises; an error the host returns rejects with an Error carrying the host's message.
declare const host: {
  Lookup(id: string): Promise<{ name: string }>;
  Value(): Promise<string>;
  Fail(msg: string): Promise<void>;
  Sum(base: number, more: number[] | null): Promise<number>;
};

class Hosted {
  constructor(private readonly origin: string) {}

  async Describe(id: string) {
    const user = await host.Lookup(id);
    user.name = "described " + user.name;
    return user;
  }

  async Relay(msg: string) {
    try {
      await host.Fail(msg);
      return "";
    } catch (err) {
      return err instanceof Error ? err.message : String(err);
    }
  }

  Value() {
    return host.Value();
  }

  Origin() {
    return this.origin;
  }

  Total(base: number, more: number[] | null) {
    return host.Sum(base, more);
  }
}

// Create calls the host once, so host calls work while Create runs.
export async function Create() {
  return new Hosted(await host.Value());
}
