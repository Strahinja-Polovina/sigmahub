-- SIGMA-215, demo side: the mirror learns what a Kubernetes cluster is.
--
-- A cluster was a control-plane-only concept here. Every call site asked
-- cpEnabled() and handed the views an empty list, so with no control plane a
-- user could not see a cluster, build one, or deploy into one — and the New
-- Resource wizard's cluster target, threaded end to end since SIGMA-200, had
-- nothing to offer.
--
-- The columns are CpCluster and CpClusterNode field for field, so listClusters
-- answers with one type in both modes and a drift between them fails type
-- checking instead of rendering wrong.
--
-- resources.cluster_id is the other half. A workload deployed into a cluster
-- has no server — the scheduler picks the node — so with only server_id to
-- write to, a demo resource the user targeted at a cluster was stored with no
-- target at all and rendered as unassigned.
CREATE TABLE "cluster_nodes" (
	"cluster_id" text NOT NULL,
	"server_id" text NOT NULL,
	"role" text DEFAULT 'worker' NOT NULL,
	"node_status" text DEFAULT 'pending' NOT NULL,
	"node_message" text DEFAULT '' NOT NULL,
	"joined_at" timestamp DEFAULT now() NOT NULL,
	"reported_at" timestamp,
	CONSTRAINT "cluster_nodes_cluster_id_server_id_pk" PRIMARY KEY("cluster_id","server_id")
);
--> statement-breakpoint
CREATE TABLE "clusters" (
	"id" text PRIMARY KEY NOT NULL,
	"org_id" text NOT NULL,
	"environment_id" text NOT NULL,
	"name" text NOT NULL,
	"status" text DEFAULT 'provisioning' NOT NULL,
	"api_endpoint" text DEFAULT '' NOT NULL,
	"kubernetes_version" text DEFAULT '' NOT NULL,
	"created_by" text DEFAULT '' NOT NULL,
	"created_at" timestamp DEFAULT now() NOT NULL
);
--> statement-breakpoint
ALTER TABLE "resources" ADD COLUMN "cluster_id" text;--> statement-breakpoint
ALTER TABLE "cluster_nodes" ADD CONSTRAINT "cluster_nodes_cluster_id_clusters_id_fk" FOREIGN KEY ("cluster_id") REFERENCES "public"."clusters"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "cluster_nodes" ADD CONSTRAINT "cluster_nodes_server_id_servers_id_fk" FOREIGN KEY ("server_id") REFERENCES "public"."servers"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "clusters" ADD CONSTRAINT "clusters_org_id_orgs_id_fk" FOREIGN KEY ("org_id") REFERENCES "public"."orgs"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "clusters" ADD CONSTRAINT "clusters_environment_id_environments_id_fk" FOREIGN KEY ("environment_id") REFERENCES "public"."environments"("id") ON DELETE cascade ON UPDATE no action;--> statement-breakpoint
ALTER TABLE "resources" ADD CONSTRAINT "resources_cluster_id_clusters_id_fk" FOREIGN KEY ("cluster_id") REFERENCES "public"."clusters"("id") ON DELETE set null ON UPDATE no action;