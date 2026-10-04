import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { ClientsPage } from "@/portal/ClientsPage";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("portal.title.clients") };
}

export default function Page() {
  return <ClientsPage />;
}
