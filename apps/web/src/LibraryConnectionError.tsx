import { Button } from "@neoread/ui/components/button";

export function LibraryConnectionError({ error }: { error: string }) {
  return (
    <div className="empty-state" role="alert">
      <h2>暂时无法连接本地书库</h2>
      <p>{error}</p>
      <Button variant="outline" onClick={() => location.reload()}>
        重新连接
      </Button>
    </div>
  );
}
