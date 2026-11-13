export type RegistryTagPage = { items: string[]; next: string };
export type RegistryTagPreview<P = RegistryTagPage> =
  | { page: P }
  | { error: unknown };

export async function loadRegistryTagPreviews<P = RegistryTagPage>(
  repositories: string[],
  fetchTags: (repository: string, signal: AbortSignal) => Promise<P>,
  signal: AbortSignal,
  onResult: (repository: string, result: RegistryTagPreview<P>) => void,
): Promise<void> {
  let index = 0;
  async function worker() {
    while (!signal.aborted && index < repositories.length) {
      const repository = repositories[index++];
      try {
        const page = await fetchTags(repository, signal);
        if (!signal.aborted) onResult(repository, { page });
      } catch (error) {
        if (!signal.aborted) onResult(repository, { error });
      }
    }
  }
  await Promise.all(
    Array.from({ length: Math.min(4, repositories.length) }, worker),
  );
}
