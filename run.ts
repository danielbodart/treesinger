#!/usr/bin/env bun
import { $ } from "bun";
import packageJson from "./package.json" with { type: "json" };

/**
 * The version, derived from the repository rather than stored in it.
 *
 * Only the major is committed (in package.json) - it is the one part that is a
 * deliberate decision. Minor is the commit count, so it only ever rises and
 * names exactly one commit. Patch is the CI run number (or a local timestamp),
 * separating two builds of the same commit - a re-run or a manual build - and
 * making a developer build sort after CI's and obviously not one.
 */
export async function version(): Promise<string> {
  const major = packageJson.version.split(".")[0];

  // Counted from HEAD, not from a branch name. On Actions the checkout is
  // detached, so `git rev-parse --abbrev-ref HEAD` answers "HEAD", and
  // GITHUB_REF_NAME on a pull request is "123/merge", which is not a rev at
  // all. HEAD is the commit being built in every one of those cases.
  if ((await $`git rev-parse --is-shallow-repository`.quiet()).text().trim() === "true") {
    throw new Error(
      "this is a shallow clone, so the commit count is wrong and so is the version. " +
        "In Actions, checkout with `fetch-depth: 0`.",
    );
  }

  const revisions = (await $`git rev-list --count HEAD`.quiet()).text().trim();
  const build = process.env.GITHUB_RUN_NUMBER ||
    new Date().toISOString().replace(/[-:T]/g, "").split(".")[0];

  return `${major}.${revisions}.${build}`;
}

/**
 * tsc's own script, run by bun. Not `bunx tsc`, which execs its
 * `#!/usr/bin/env node` shebang: there may be no node (a mise host pinning
 * only bun), and in the Nix sandbox there is no /usr/bin/env either.
 */
export async function check() {
  await $`bun node_modules/typescript/bin/tsc --noEmit`;
}

export async function test(...args: string[]) {
  await $`bun test ${args}`;
}

/**
 * The version reaches the binary as a build-time define rather than a generated
 * file, so nothing is written into src/ that would then show up as a change.
 * src/version.ts falls back when the define is absent, which is what happens
 * under `bun run src/main.ts`.
 *
 * A compiled binary would otherwise read a .env and bunfig.toml from whatever
 * directory it is run in: `sudo -u treesinger treesinger login` from a project
 * directory could pick up someone else's TREESINGER_STORE.
 */
async function compile(v: string, target: string, outfile: string) {
  await $`bun build --compile --no-compile-autoload-dotenv --no-compile-autoload-bunfig --minify --sourcemap --target=${target} --define ${`TREESINGER_VERSION="${v}"`} src/main.ts --outfile ${outfile}`;
}

export async function build(outfile = "dist/treesinger") {
  const v = await version();
  await compile(v, "bun-linux-x64", outfile);
  console.log(`built ${outfile} ${v}`);
}

/**
 * What a GitHub release carries: a binary per architecture, the plain systemd
 * unit and sysusers file that run it, the client helper, and their checksums,
 * all in one directory for CI to upload as they are. The binaries are glibc
 * builds, for the distributions a native install is for; the image's musl
 * build is for Alpine.
 *
 * x64 is Bun's baseline build, which runs without AVX2. The low-power Celerons
 * and Atoms in a lot of home servers do not have it.
 */
export async function release(dir = "dist/release") {
  const v = await version();
  await $`rm -rf ${dir} && mkdir -p ${dir}`;
  for (const [arch, target] of [["x64", "bun-linux-x64-baseline"], ["arm64", "bun-linux-arm64"]]) {
    await compile(v, target, `${dir}/treesinger-linux-${arch}`);
  }
  // The map is in the binary already; the copy beside it is not an asset.
  await $`rm -f ${dir}/main.js.map`;
  await $`cp contrib/systemd/treesinger.service contrib/systemd/treesinger.sysusers contrib/treesinger-env ${dir}/`;
  await $`sha256sum treesinger-linux-x64 treesinger-linux-arm64 treesinger.service treesinger.sysusers treesinger-env > SHA256SUMS`.cwd(dir);
  console.log(`built ${dir} ${v}`);
}

/** The same image CI publishes, for both architectures, built locally. */
export async function image(tag = "ghcr.io/danielbodart/treesinger:dev") {
  const v = await version();
  await $`docker buildx build --platform linux/amd64,linux/arm64 --build-arg VERSION=${v} --tag ${tag} .`;
}

const commands: Record<string, (...args: string[]) => Promise<unknown>> = {
  version: async () => console.log(await version()),
  check,
  test,
  build,
  release,
  image,
};

const [name = "build", ...args] = process.argv.slice(2);
const command = commands[name];

if (!command) {
  console.error(`Error: '${name}' is not a command. Try: ${Object.keys(commands).join(", ")}`);
  process.exit(2);
}

try {
  await command(...args);
} catch (error) {
  console.error(`Error: ${(error as Error).message}`);
  process.exit(1);
}
