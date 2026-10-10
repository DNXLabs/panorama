# SPDX-FileCopyrightText: 2026 DNX Technology Pty Ltd and contributors
# SPDX-License-Identifier: AGPL-3.0-only

"""What deploy/cloudformation/panorama.yaml stands up: the diagram on "Deploying on AWS".

Two questions it answers before anybody runs the template: which parts are mine
to bring (the VPC, the subnets, the certificate) and which does the stack make,
and what do the tasks have to be able to reach. Every node is something the
template creates or takes as a parameter; if the template changes, this does.

Four rows: what arrives, the load balancer, the tasks beside their database,
and the AWS services the tasks call out to.
"""

from diagrams.aws.compute import ECR, Fargate
from diagrams.aws.database import Aurora
from diagrams.aws.management import CloudwatchLogs
from diagrams.aws.ml import Bedrock
from diagrams.aws.network import ALB, Route53
from diagrams.aws.security import ACM, SecretsManager
from diagrams.onprem.client import Users

from theme import THEMES, cluster, diagram, edge, mono, same_rank

for t in THEMES:
    with diagram("aws-architecture", t, direction="TB", ranksep="0.7", nodesep="0.45"):
        people = mono(Users, t)("Coding tools\nand console users")

        with cluster("You bring", t):
            dns = Route53("Route 53 alias\noptional")
            cert = ACM("ACM certificate")

        with cluster("Your VPC  ·  you bring it, and the subnets", t, accent=True):
            with cluster("Load balancer subnets", t, inset=True):
                alb = ALB("Application Load Balancer\nHTTPS, idle timeout 600s")
            with cluster("Service subnets  ·  two availability zones", t, inset=True) as svc:
                tasks = [Fargate("Fargate task"), Fargate("Fargate task")]
                db = Aurora("Aurora Serverless v2\nPostgreSQL")
                same_rank(svc, [*tasks, db])

        with cluster("Reached through a NAT gateway or VPC endpoints", t):
            bedrock = Bedrock("Amazon Bedrock\nruntime and mantle")
            secrets = SecretsManager("Secrets Manager\ndatabase, key, OIDC")
            logs = CloudwatchLogs("CloudWatch Logs")
            ecr = ECR("ECR Public image\nor your mirror")

        people >> edge(t, accent=True) >> alb
        dns >> edge(t, dashed=True) >> alb
        cert >> edge(t, "TLS", dashed=True) >> alb
        for task in tasks:
            alb >> edge(t, accent=True) >> task
        tasks[1] >> edge(t) >> db

        tasks[0] >> edge(t, "task role", accent=True) >> bedrock
        tasks[0] >> edge(t, dashed=True) >> secrets
        tasks[1] >> edge(t, dashed=True) >> logs
        tasks[1] >> edge(t, "execution role", dashed=True) >> ecr
