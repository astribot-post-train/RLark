export function imageRegistryReference(
  registry: string,
  repository: string,
  tag: string,
): string {
  return `${registry}/${repository}:${tag}`;
}

export function nextRegistryPage(cursors: string[], next: string): string[] {
  return next && !cursors.includes(next) ? [...cursors, next] : cursors;
}

export function previousRegistryPage(cursors: string[]): string[] {
  return cursors.length > 1 ? cursors.slice(0, -1) : cursors;
}
